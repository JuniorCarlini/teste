package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// A API do Easypanel mudou de forma três vezes (tRPC até a 2.30, RPC na
// 2.31–2.32, API pública a partir da 2.33). O programa só precisa de uma
// leitura sem parâmetros, listProjectsAndServices, e tenta as três formas.
// A API é opcional: sem ela o inventário sai só do Docker, e o que se perde
// é a origem Git das apps (repositório e branch).

type apiDados struct {
	versao   string
	servicos []apiServico
}

type apiServico struct {
	projeto, nome, tipo string
	bruto               map[string]any
}

func lerAPI(ctx context.Context, base, token string) (*apiDados, error) {
	base = strings.TrimRight(base, "/")
	cli := &http.Client{Timeout: 30 * time.Second}
	get := func(method, path string, body []byte) (any, int, error) {
		req, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(body))
		if err != nil {
			return nil, 0, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		res, err := cli.Do(req)
		if err != nil {
			return nil, 0, err
		}
		defer func() { _ = res.Body.Close() }()
		b, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
		if err != nil {
			return nil, res.StatusCode, err
		}
		var v any
		if err := json.Unmarshal(b, &v); err != nil {
			return nil, res.StatusCode, fmt.Errorf("resposta não é JSON (HTTP %d)", res.StatusCode)
		}
		return v, res.StatusCode, nil
	}

	d := &apiDados{}
	if v, code, err := get("GET", "/api/getUpdateStatus", nil); err == nil && code == 200 {
		if m, ok := v.(map[string]any); ok {
			d.versao, _ = m["version"].(string)
		}
	}

	var ultimo error
	tentativas := []struct {
		method, path string
		body         []byte
		desembrulhar func(any) any
	}{
		{"GET", "/api/listProjectsAndServices", nil, func(v any) any { return v }},
		{"POST", "/api/rpc/projects/listProjectsAndServices", []byte(`{"json":{}}`), func(v any) any { return campo(v, "json") }},
		{"GET", "/api/trpc/projects.listProjectsAndServices", nil, func(v any) any { return campo(campo(campo(v, "result"), "data"), "json") }},
	}
	for _, t := range tentativas {
		v, code, err := get(t.method, t.path, t.body)
		if err != nil {
			ultimo = err
			continue
		}
		if code == 401 || code == 403 {
			return nil, errors.New("token recusado pelo Easypanel (gere um em Configurações › Usuários › API)")
		}
		if code != 200 {
			ultimo = fmt.Errorf("%s respondeu HTTP %d", t.path, code)
			continue
		}
		if d.servicos = extrairServicos(t.desembrulhar(v)); len(d.servicos) > 0 {
			return d, nil
		}
		ultimo = fmt.Errorf("%s não trouxe serviços", t.path)
	}
	return nil, ultimo
}

func campo(v any, k string) any {
	if m, ok := v.(map[string]any); ok {
		return m[k]
	}
	return nil
}

func texto(v any, ks ...string) string {
	for _, k := range ks {
		if s, ok := campo(v, k).(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// extrairServicos aceita as formas conhecidas da resposta:
// {services:[{projectName,name,type,...}]}, {projects:[{name,services:[...]}]}.
func extrairServicos(v any) []apiServico {
	var out []apiServico
	add := func(proj string, s any) {
		m, ok := s.(map[string]any)
		if !ok {
			return
		}
		p := texto(m, "projectName", "project")
		if p == "" {
			p = proj
		}
		n := texto(m, "name", "serviceName")
		if p != "" && n != "" {
			out = append(out, apiServico{projeto: p, nome: n, tipo: texto(m, "type"), bruto: m})
		}
	}
	if l, ok := campo(v, "services").([]any); ok {
		for _, s := range l {
			add("", s)
		}
	}
	if l, ok := campo(v, "projects").([]any); ok {
		for _, p := range l {
			if ss, ok := campo(p, "services").([]any); ok {
				for _, s := range ss {
					add(texto(p, "name", "projectName"), s)
				}
			}
		}
	}
	return out
}

func (d *apiDados) separar(swarm string) (string, string, bool) {
	for _, s := range d.servicos {
		if s.projeto+"_"+s.nome == swarm {
			return s.projeto, s.nome, true
		}
	}
	return "", "", false
}

// enriquecer completa o serviço com o que só a API sabe: a origem Git e o
// tipo declarado no Easypanel.
func (d *apiDados) enriquecer(proj string, s *Servico) {
	for _, a := range d.servicos {
		if a.projeto != proj || a.nome != s.Nome {
			continue
		}
		if a.tipo != "" && a.tipo != "app" && a.tipo != "compose" {
			s.Tipo = a.tipo
		}
		s.Git = fonteGit(a.bruto)
		if s.PortaInterna == 0 {
			s.PortaInterna = portaDaAPI(a.bruto)
		}
		return
	}
}

// fonteGit lê "source" (github: owner/repo/ref/path; git: repo/ref/path) e
// "build" (type, file).
func fonteGit(m map[string]any) *FonteGit {
	src, ok := m["source"].(map[string]any)
	if !ok {
		return nil
	}
	f := &FonteGit{Tipo: texto(src, "type")}
	switch f.Tipo {
	case "github":
		owner, repo := texto(src, "owner"), texto(src, "repo")
		if owner == "" || repo == "" {
			return nil
		}
		f.Repo = "https://github.com/" + owner + "/" + repo
	case "git":
		f.Repo = texto(src, "repo", "url")
		if f.Repo == "" {
			return nil
		}
	default:
		return nil
	}
	f.Branch = texto(src, "ref", "branch")
	f.Pasta = strings.Trim(texto(src, "path"), "/")
	if b, ok := m["build"].(map[string]any); ok {
		f.Build = texto(b, "type")
		f.Dockerfile = texto(b, "file")
	}
	return f
}

func portaDaAPI(m map[string]any) int {
	ds, _ := m["domains"].([]any)
	for _, d := range ds {
		if p, ok := campo(d, "port").(float64); ok && p > 0 {
			return int(p)
		}
	}
	return 0
}
