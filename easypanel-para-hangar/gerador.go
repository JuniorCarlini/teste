package main

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// O gerador escreve, para cada projeto, um compose.yaml no formato que a aba
// "Compose" do Hangar importa (docs/27 do Hangar): PostgreSQL e Redis viram
// templates (senha nova, backups, referências ${{db.DATABASE_URL}}), o resto
// vira serviço por imagem com as mesmas variáveis e discos.

// RegistroMigracao é o registro inválido de propósito (RFC 2606) usado nas
// imagens construídas pelo Easypanel: o Hangar tenta baixar, o nome nunca
// resolve, e ele usa a imagem que já está no servidor. Um nome sem registro
// (ex.: "migracao/loja-web") iria para o Docker Hub, onde qualquer um poderia
// publicar uma imagem com esse nome.
const RegistroMigracao = "ep2hangar.invalid"

const tagMigracao = "migracao"

// ImagemMigracao é o nome com que a imagem local de um serviço é guardada.
func ImagemMigracao(p *Projeto, s *Servico) string {
	return RegistroMigracao + "/" + p.Slug + "-" + s.Slug + ":" + tagMigracao
}

type composeArquivo struct {
	Services map[string]*composeServico `yaml:"services"`
	Volumes  map[string]struct{}        `yaml:"volumes,omitempty"`
}

type composeServico struct {
	Image       string            `yaml:"image"`
	Command     []string          `yaml:"command,omitempty,flow"`
	Environment map[string]string `yaml:"environment,omitempty"`
	Volumes     []string          `yaml:"volumes,omitempty"`
	Expose      []string          `yaml:"expose,omitempty"`
	Deploy      *composeDeploy    `yaml:"deploy,omitempty"`
}

type composeDeploy struct {
	Resources struct {
		Limits map[string]string `yaml:"limits"`
	} `yaml:"resources"`
}

// Plano é o que o gerador decidiu para um projeto: o compose e as pendências
// que o operador resolve à mão no Hangar.
type Plano struct {
	Projeto     *Projeto
	Compose     []byte
	Pendencias  []string
	Dominios    []string
	Portas      []string
	Git         []string
	Restauracao []string
}

var hostRE2 = regexp.MustCompile(`[A-Za-z0-9_.-]+`)

// GerarPlano preenche Montagem.Disco nos serviços do projeto (a restauração
// usa esse nome) e devolve o compose.
// globais são os endereços internos de todos os projetos ("loja_db").
func GerarPlano(p *Projeto, globais map[string]bool) (*Plano, error) {
	pl := &Plano{Projeto: p}
	cf := composeArquivo{Services: map[string]*composeServico{}, Volumes: map[string]struct{}{}}

	// Endereço interno no Easypanel ("loja_db") → nome no compose ("db"):
	// é por esse nome que o Hangar liga a variável ao banco importado.
	hosts := map[string]string{}
	usados := map[string]string{}
	for _, s := range p.Servicos {
		if s.Slug == "" {
			return nil, fmt.Errorf("serviço %q: nome não vira um endereço válido no Hangar", s.Nome)
		}
		if outro, ok := usados[s.Slug]; ok {
			return nil, fmt.Errorf("serviços %q e %q viram o mesmo endereço %q no Hangar", outro, s.Nome, s.Slug)
		}
		usados[s.Slug] = s.Nome
		hosts[p.Nome+"_"+s.Nome] = s.Slug
	}
	trocar := func(v string) string {
		return hostRE2.ReplaceAllStringFunc(v, func(h string) string {
			if n, ok := hosts[h]; ok {
				return n
			}
			return h
		})
	}

	for i := range p.Servicos {
		s := &p.Servicos[i]
		if s.Tipo == "compose" {
			pl.Pendencias = append(pl.Pendencias, fmt.Sprintf("**%s** é um serviço Compose do Easypanel: importe o arquivo de `compose/%s/` na aba Compose do Hangar (ele é lido como está, sem conversão).", s.Nome, s.Slug))
			continue
		}
		cs := &composeServico{Environment: map[string]string{}}
		cf.Services[s.Slug] = cs

		cs.Image = s.Imagem
		if s.ImagemLocal {
			cs.Image = ImagemMigracao(p, s)
		}
		env := s.EnvMap()
		for k, v := range env {
			cs.Environment[k] = escapar(trocar(v))
		}

		switch s.Tipo {
		case "postgres":
			// O template cria o banco com a senha nova; os dados voltam pelo dump.
			pl.Restauracao = append(pl.Restauracao, fmt.Sprintf("`%s`: dump do PostgreSQL (`pg_restore`)", s.Slug))
			for _, k := range []string{"POSTGRES_DB", "POSTGRES_USER", "POSTGRES_PASSWORD"} {
				if v, ok := env[k]; ok {
					cs.Environment[k] = escapar(v)
				}
			}
			for k := range cs.Environment {
				if !strings.HasPrefix(k, "POSTGRES_") {
					delete(cs.Environment, k)
				}
			}
			cs.Volumes = []string{"pg-" + s.Slug + ":/var/lib/postgresql"}
			cf.Volumes["pg-"+s.Slug] = struct{}{}
			for j := range s.Montagens {
				s.Montagens[j].Disco = ""
			}
			limites(cs, s)
			continue
		case "redis":
			// O template gera a senha e lê o modo pela persistência.
			cs.Environment = nil
			if strings.Contains(strings.Join(s.Comando, " "), "--appendonly yes") {
				cs.Command = []string{"redis-server", "--appendonly", "yes"}
			}
			cs.Volumes = []string{"redis-" + s.Slug + ":/data"}
			cf.Volumes["redis-"+s.Slug] = struct{}{}
			for j := range s.Montagens {
				if path.Clean(s.Montagens[j].Destino) == "/data" {
					s.Montagens[j].Disco = "data" // disco do template
				}
			}
			pl.Restauracao = append(pl.Restauracao, fmt.Sprintf("`%s`: arquivos de /data do Redis", s.Slug))
			limites(cs, s)
			continue
		}

		cs.Command = s.Comando
		discos := map[string]bool{}
		for j := range s.Montagens {
			m := &s.Montagens[j]
			switch {
			case m.Tipo != "volume" && m.Tipo != "bind":
				continue
			case m.Ausente:
				pl.Pendencias = append(pl.Pendencias, fmt.Sprintf("**%s**: a pasta `%s` (montada em `%s`) não existe mais no servidor; ficou de fora.", s.Nome, m.Origem, m.Destino))
				continue
			case m.Arquivo:
				pl.Pendencias = append(pl.Pendencias, fmt.Sprintf("**%s**: arquivo montado em `%s` (File Mount do Easypanel). O Hangar não monta arquivos: o conteúdo está em `dados/%s/%s/arquivos/`; passe para uma variável ou para a imagem.", s.Nome, m.Destino, p.Slug, s.Slug))
				continue
			case m.Destino == "/var/run/docker.sock" || m.Origem == "/var/run/docker.sock":
				pl.Pendencias = append(pl.Pendencias, fmt.Sprintf("**%s**: monta o socket do Docker; o Hangar nunca faz isso. O serviço sobe sem ele.", s.Nome))
				continue
			}
			m.Disco = nomeDisco(m.Destino, discos)
			discos[m.Disco] = true
			if len(discos) > 8 {
				pl.Pendencias = append(pl.Pendencias, fmt.Sprintf("**%s**: mais de 8 discos; o Hangar aceita 8. `%s` ficou de fora.", s.Nome, m.Destino))
				m.Disco = ""
				continue
			}
			ro := ""
			if m.SomenteLeitura {
				ro = ":ro"
			}
			cs.Volumes = append(cs.Volumes, m.Disco+":"+m.Destino+ro)
			cf.Volumes[m.Disco] = struct{}{}
			if m.Tipo == "bind" {
				pl.Restauracao = append(pl.Restauracao, fmt.Sprintf("`%s`: pasta `%s` do servidor → disco `%s`", s.Slug, m.Origem, m.Disco))
			} else {
				pl.Restauracao = append(pl.Restauracao, fmt.Sprintf("`%s`: volume `%s` → disco `%s`", s.Slug, m.Origem, m.Disco))
			}
		}
		if s.PortaInterna > 0 {
			cs.Expose = []string{strconv.Itoa(s.PortaInterna)}
		}
		limites(cs, s)

		if s.Replicas == 0 {
			pl.Pendencias = append(pl.Pendencias, fmt.Sprintf("**%s** estava desligado no Easypanel (0 réplicas): depois de importar, pare-o no Hangar se ele deve continuar parado.", s.Nome))
		} else if s.Replicas > 1 {
			pl.Pendencias = append(pl.Pendencias, fmt.Sprintf("**%s** rodava com %d réplicas; no Hangar é um container por serviço.", s.Nome, s.Replicas))
		}
		if s.ImagemLocal {
			if s.Git != nil {
				g := fmt.Sprintf("**%s**: `%s`", s.Nome, s.Git.Repo)
				if s.Git.Branch != "" {
					g += " branch `" + s.Git.Branch + "`"
				}
				if s.Git.Pasta != "" {
					g += " pasta `" + s.Git.Pasta + "`"
				}
				if s.Git.Dockerfile != "" {
					g += " Dockerfile `" + s.Git.Dockerfile + "`"
				}
				if s.Git.Build != "" && s.Git.Build != "dockerfile" {
					g += fmt.Sprintf(" (build **%s** no Easypanel: o Hangar constrói com Dockerfile; crie um antes de trocar)", s.Git.Build)
				}
				pl.Git = append(pl.Git, g)
			} else {
				pl.Git = append(pl.Git, fmt.Sprintf("**%s**: imagem construída no Easypanel; origem Git desconhecida (rode com `--api` para descobrir).", s.Nome))
			}
		}
		for _, pp := range s.PortasPublicadas {
			pl.Portas = append(pl.Portas, fmt.Sprintf("**%s**: %d → %d/%s", s.Nome, pp.Publicada, pp.Container, pp.Protocolo))
		}
		for _, d := range s.Dominios {
			linha := fmt.Sprintf("**%s**: `%s%s`", s.Nome, d.Host, d.Caminho)
			if d.Porta > 0 {
				linha += fmt.Sprintf(" → porta %d", d.Porta)
			}
			pl.Dominios = append(pl.Dominios, linha)
		}
		for k, v := range env {
			for _, h := range hostRE2.FindAllString(v, -1) {
				if globais[h] && hosts[h] == "" {
					pl.Pendencias = append(pl.Pendencias, fmt.Sprintf("**%s**: `%s` aponta para `%s`, de outro projeto. No Hangar, conecte os dois projetos (aba Rede) e confira o endereço.", s.Nome, k, h))
				}
			}
		}
	}

	if len(cf.Services) == 0 {
		return pl, nil
	}
	if len(cf.Services) > 20 {
		pl.Pendencias = append(pl.Pendencias, "O projeto tem mais de 20 serviços; o Hangar importa até 20 por vez: divida o compose.yaml em dois.")
	}
	b, err := yaml.Marshal(cf)
	if err != nil {
		return nil, err
	}
	pl.Compose = append([]byte("# Gerado pelo easypanel-para-hangar a partir do projeto \""+p.Nome+"\" do Easypanel.\n# Cole na aba Compose de um projeto novo do Hangar (Analisar → Criar).\n"), b...)
	sort.Strings(pl.Pendencias)
	return pl, nil
}

func limites(cs *composeServico, s *Servico) {
	if s.MemoriaMB == 0 && s.CPUs == 0 {
		return
	}
	cs.Deploy = &composeDeploy{}
	cs.Deploy.Resources.Limits = map[string]string{}
	if s.MemoriaMB > 0 {
		cs.Deploy.Resources.Limits["memory"] = strconv.FormatInt(s.MemoriaMB, 10) + "M"
	}
	if s.CPUs > 0 {
		cs.Deploy.Resources.Limits["cpus"] = strconv.FormatFloat(s.CPUs, 'f', -1, 64)
	}
}

// escapar: o Hangar interpreta ${VAR} no compose; um "$" de uma senha tem
// que ir como "$$".
func escapar(v string) string { return strings.ReplaceAll(v, "$", "$$") }

var discoRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,29}$`)

// nomeDisco: o nome do disco no Hangar sai da pasta montada ("/app/media" →
// "media"), único no serviço.
func nomeDisco(destino string, usados map[string]bool) string {
	n := Slugify(path.Base(path.Clean(destino)))
	if n == "" || n[0] < 'a' || n[0] > 'z' {
		n = "d-" + n
	}
	if len(n) > 26 {
		n = strings.TrimRight(n[:26], "-")
	}
	if !discoRE.MatchString(n) {
		n = "data"
	}
	base := n
	for i := 2; usados[n]; i++ {
		n = base + "-" + strconv.Itoa(i)
	}
	return n
}

var acentos = strings.NewReplacer(
	"á", "a", "à", "a", "â", "a", "ã", "a", "ä", "a",
	"é", "e", "è", "e", "ê", "e", "ë", "e",
	"í", "i", "ì", "i", "î", "i", "ï", "i",
	"ó", "o", "ò", "o", "ô", "o", "õ", "o", "ö", "o",
	"ú", "u", "ù", "u", "û", "u", "ü", "u", "ç", "c", "ñ", "n",
)

var slugRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}[a-z0-9]$`)

// Slugify segue a regra do Hangar (internal/projects/naming.go): minúsculas,
// sem acento, o resto vira hífen, até 32 caracteres. "_" vira "-".
func Slugify(nome string) string {
	s := acentos.Replace(strings.ToLower(strings.TrimSpace(nome)))
	var b strings.Builder
	dash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case b.Len() > 0 && !dash:
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out != "" && out[0] >= '0' && out[0] <= '9' {
		out = "s-" + out
	}
	if len(out) > 32 {
		out = strings.TrimRight(out[:32], "-")
	}
	if !slugRE.MatchString(out) {
		return ""
	}
	return out
}
