package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Inventario é tudo o que o programa descobriu no servidor do Easypanel. É
// gravado como inventario.json no pacote e é a única fonte do gerador e da
// restauração: nada é lido de novo do Easypanel depois da exportação.
type Inventario struct {
	GeradoEm      time.Time `json:"gerado_em"`
	Host          string    `json:"host"`
	DockerVersao  string    `json:"docker_versao"`
	Swarm         bool      `json:"swarm"`
	APIUsada      bool      `json:"api_usada"`
	Projetos      []Projeto `json:"projetos"`
	Avisos        []string  `json:"avisos,omitempty"`
	DaemonJSON    bool      `json:"daemon_json"`
	Portas80e443  bool      `json:"portas_80_443_em_uso"`
	TraefikLido   bool      `json:"traefik_lido"`
	EasypanelVers string    `json:"easypanel_versao,omitempty"`
}

type Projeto struct {
	Nome     string    `json:"nome"`
	Slug     string    `json:"slug"`
	Servicos []Servico `json:"servicos"`
}

type Servico struct {
	Nome      string `json:"nome"`
	Slug      string `json:"slug"`
	Tipo      string `json:"tipo"` // app, postgres, mysql, mariadb, mongo, redis, compose
	SwarmNome string `json:"swarm_nome,omitempty"`
	Imagem    string `json:"imagem,omitempty"`
	// ImagemLocal: imagem construída no próprio servidor (build do
	// Easypanel); não existe em registro nenhum.
	ImagemLocal      bool              `json:"imagem_local,omitempty"`
	Env              []EnvVar          `json:"env,omitempty"`
	Comando          []string          `json:"comando,omitempty"`
	Montagens        []Montagem        `json:"montagens,omitempty"`
	MemoriaMB        int64             `json:"memoria_mb,omitempty"`
	CPUs             float64           `json:"cpus,omitempty"`
	Replicas         uint64            `json:"replicas"`
	PortasPublicadas []Porta           `json:"portas_publicadas,omitempty"`
	Dominios         []Dominio         `json:"dominios,omitempty"`
	PortaInterna     int               `json:"porta_interna,omitempty"`
	Git              *FonteGit         `json:"git,omitempty"`
	ComposeArquivos  map[string]string `json:"compose_arquivos,omitempty"`
	Notas            []string          `json:"notas,omitempty"`
}

type EnvVar struct {
	Nome  string `json:"nome"`
	Valor string `json:"valor"`
}

type Montagem struct {
	Tipo           string `json:"tipo"` // volume | bind
	Origem         string `json:"origem"`
	Destino        string `json:"destino"`
	SomenteLeitura bool   `json:"somente_leitura,omitempty"`
	// Arquivo: bind de um arquivo (File Mount do Easypanel), não de pasta.
	Arquivo bool `json:"arquivo,omitempty"`
	// Ausente: o caminho não existe mais no servidor.
	Ausente bool `json:"ausente,omitempty"`
	// Disco é o nome do disco no Hangar (definido pelo gerador).
	Disco string `json:"disco,omitempty"`
}

type Porta struct {
	Publicada int    `json:"publicada"`
	Container int    `json:"container"`
	Protocolo string `json:"protocolo"`
}

type Dominio struct {
	Host    string `json:"host"`
	Caminho string `json:"caminho,omitempty"`
	Porta   int    `json:"porta,omitempty"`
}

type FonteGit struct {
	Tipo       string `json:"tipo"` // github | git
	Repo       string `json:"repo"`
	Branch     string `json:"branch,omitempty"`
	Pasta      string `json:"pasta,omitempty"`
	Dockerfile string `json:"dockerfile,omitempty"`
	Build      string `json:"build,omitempty"` // dockerfile, nixpacks, heroku-buildpacks...
}

func (s *Servico) Banco() bool {
	switch s.Tipo {
	case "postgres", "mysql", "mariadb", "mongo", "redis":
		return true
	}
	return false
}

func (s *Servico) EnvMap() map[string]string {
	m := map[string]string{}
	for _, e := range s.Env {
		m[e.Nome] = e.Valor
	}
	return m
}

// --- coleta ---------------------------------------------------------------

type OpcoesInventario struct {
	APIURL   string
	APIToken string
	Projetos []string // vazio = todos
}

// swarmService é o pedaço de `docker service inspect` que interessa.
type swarmService struct {
	Spec struct {
		Name         string            `json:"Name"`
		Labels       map[string]string `json:"Labels"`
		TaskTemplate struct {
			ContainerSpec struct {
				Image   string   `json:"Image"`
				Env     []string `json:"Env"`
				Command []string `json:"Command"`
				Args    []string `json:"Args"`
				Mounts  []struct {
					Type     string `json:"Type"`
					Source   string `json:"Source"`
					Target   string `json:"Target"`
					ReadOnly bool   `json:"ReadOnly"`
				} `json:"Mounts"`
			} `json:"ContainerSpec"`
			Resources struct {
				Limits struct {
					NanoCPUs    int64 `json:"NanoCPUs"`
					MemoryBytes int64 `json:"MemoryBytes"`
				} `json:"Limits"`
			} `json:"Resources"`
		} `json:"TaskTemplate"`
		Mode struct {
			Replicated *struct {
				Replicas uint64 `json:"Replicas"`
			} `json:"Replicated"`
		} `json:"Mode"`
		EndpointSpec struct {
			Ports []struct {
				Protocol      string `json:"Protocol"`
				TargetPort    int    `json:"TargetPort"`
				PublishedPort int    `json:"PublishedPort"`
			} `json:"Ports"`
		} `json:"EndpointSpec"`
	} `json:"Spec"`
}

// Coletar monta o inventário a partir do Docker do servidor (fonte de
// verdade: imagem, variáveis, discos, recursos), do Traefik do Easypanel
// (domínios) e, se houver token, da API do Easypanel (origem Git).
func Coletar(ctx context.Context, d Docker, op OpcoesInventario) (*Inventario, error) {
	inv := &Inventario{GeradoEm: time.Now().UTC(), Host: d.Host}
	if inv.Host == "" {
		inv.Host = "local"
	}
	if v, err := d.Run(ctx, "version", "--format", "{{.Server.Version}}"); err == nil {
		inv.DockerVersao = strings.TrimSpace(string(v))
	} else {
		return nil, fmt.Errorf("não consegui falar com o Docker do servidor: %w", err)
	}
	if v, err := d.Run(ctx, "info", "--format", "{{.Swarm.LocalNodeState}}"); err == nil {
		inv.Swarm = strings.TrimSpace(string(v)) == "active"
	}
	if !inv.Swarm {
		inv.Avisos = append(inv.Avisos, "o Docker deste servidor não está em modo Swarm: não parece um servidor com Easypanel")
	}

	projetos := map[string]*Projeto{}
	pegar := func(nome string) *Projeto {
		p, ok := projetos[nome]
		if !ok {
			p = &Projeto{Nome: nome, Slug: Slugify(nome)}
			projetos[nome] = p
		}
		return p
	}

	// Pares projeto/serviço vindos da API resolvem nomes com "_" sem chute.
	var api *apiDados
	if op.APIURL != "" {
		var err error
		api, err = lerAPI(ctx, op.APIURL, op.APIToken)
		if err != nil {
			inv.Avisos = append(inv.Avisos, "API do Easypanel não lida (seguindo só com o Docker): "+err.Error())
		} else {
			inv.APIUsada = true
			inv.EasypanelVers = api.versao
		}
	}

	if inv.Swarm {
		ids, err := d.Lines(ctx, "service", "ls", "-q")
		if err != nil {
			return nil, err
		}
		if len(ids) > 0 {
			var svcs []swarmService
			if err := d.JSON(ctx, &svcs, append([]string{"service", "inspect"}, ids...)...); err != nil {
				return nil, err
			}
			for _, sw := range svcs {
				proj, nome, ok := separarNome(sw.Spec.Name, api)
				if !ok {
					continue
				}
				s := servicoDoSwarm(sw)
				s.Nome, s.Slug = nome, Slugify(nome)
				if api != nil {
					api.enriquecer(proj, &s)
				}
				p := pegar(proj)
				p.Servicos = append(p.Servicos, s)
			}
		}
	}

	if err := coletarCompose(ctx, d, pegar, inv); err != nil {
		inv.Avisos = append(inv.Avisos, "serviços do tipo Compose não lidos: "+err.Error())
	}

	// Domínios: configuração dinâmica que o Easypanel escreve para o Traefik.
	if files, err := d.ReadHostDir(ctx, "/etc/easypanel/traefik/config", 8<<20); err != nil {
		inv.Avisos = append(inv.Avisos, "configuração do Traefik não lida (domínios ficam de fora): "+err.Error())
	} else if len(files) > 0 {
		inv.TraefikLido = true
		rotas := lerTraefik(files)
		for _, p := range projetos {
			for i := range p.Servicos {
				s := &p.Servicos[i]
				for _, r := range rotas[p.Nome+"_"+s.Nome] {
					s.Dominios = append(s.Dominios, Dominio{Host: r.Host, Caminho: r.Caminho, Porta: r.Porta})
					if s.PortaInterna == 0 && r.Porta > 0 {
						s.PortaInterna = r.Porta
					}
				}
			}
		}
	}

	if err := marcarArquivos(ctx, d, projetos); err != nil {
		inv.Avisos = append(inv.Avisos, "não consegui conferir as pastas montadas: "+err.Error())
	}

	if _, err := d.Run(ctx, "run", "--rm", "--network", "none", "--mount", "type=bind,src=/etc/docker,dst=/d,readonly", d.Auxiliar, "test", "-f", "/d/daemon.json"); err == nil {
		inv.DaemonJSON = true
	}
	inv.Portas80e443 = portasEmUso(ctx, d)

	filtro := map[string]bool{}
	for _, p := range op.Projetos {
		filtro[p] = true
	}
	for nome, p := range projetos {
		if len(filtro) > 0 && !filtro[nome] && !filtro[p.Slug] {
			continue
		}
		sort.Slice(p.Servicos, func(i, j int) bool { return p.Servicos[i].Nome < p.Servicos[j].Nome })
		inv.Projetos = append(inv.Projetos, *p)
	}
	sort.Slice(inv.Projetos, func(i, j int) bool { return inv.Projetos[i].Nome < inv.Projetos[j].Nome })
	return inv, nil
}

// separarNome divide "<projeto>_<serviço>". Serviços do próprio Easypanel
// (easypanel, easypanel-traefik) ficam de fora.
func separarNome(nome string, api *apiDados) (proj, svc string, ok bool) {
	if strings.HasPrefix(nome, "easypanel") {
		return "", "", false
	}
	if api != nil {
		if p, s, ok := api.separar(nome); ok {
			return p, s, true
		}
	}
	i := strings.Index(nome, "_")
	if i <= 0 || i == len(nome)-1 {
		return "", "", false
	}
	return nome[:i], nome[i+1:], true
}

func servicoDoSwarm(sw swarmService) Servico {
	cs := sw.Spec.TaskTemplate.ContainerSpec
	s := Servico{SwarmNome: sw.Spec.Name, Replicas: 1}
	s.Imagem = semDigest(cs.Image)
	s.ImagemLocal = imagemLocal(s.Imagem)
	s.Tipo = tipoPelaImagem(s.Imagem)
	for _, kv := range cs.Env {
		k, v, _ := strings.Cut(kv, "=")
		s.Env = append(s.Env, EnvVar{Nome: k, Valor: v})
	}
	s.Comando = append(append(s.Comando, cs.Command...), cs.Args...)
	for _, m := range cs.Mounts {
		s.Montagens = append(s.Montagens, Montagem{Tipo: strings.ToLower(m.Type), Origem: m.Source, Destino: m.Target, SomenteLeitura: m.ReadOnly})
	}
	lim := sw.Spec.TaskTemplate.Resources.Limits
	if lim.MemoryBytes > 0 {
		s.MemoriaMB = lim.MemoryBytes / (1 << 20)
	}
	if lim.NanoCPUs > 0 {
		s.CPUs = float64(lim.NanoCPUs) / 1e9
	}
	if r := sw.Spec.Mode.Replicated; r != nil {
		s.Replicas = r.Replicas
	}
	for _, p := range sw.Spec.EndpointSpec.Ports {
		s.PortasPublicadas = append(s.PortasPublicadas, Porta{Publicada: p.PublishedPort, Container: p.TargetPort, Protocolo: strings.ToLower(p.Protocol)})
	}
	return s
}

func semDigest(img string) string {
	if i := strings.Index(img, "@sha256:"); i >= 0 {
		img = img[:i]
	}
	return img
}

// imagemLocal: o Easypanel marca as imagens que constrói como easypanel/<p>/<s>.
func imagemLocal(img string) bool {
	return strings.HasPrefix(img, "easypanel/")
}

// baseImagem devolve o nome da imagem sem registro, dono e tag:
// "docker.io/library/postgres:16" → "postgres".
func baseImagem(img string) string {
	img = semDigest(img)
	if i := strings.LastIndex(img, "/"); i >= 0 {
		img = img[i+1:]
	}
	if i := strings.Index(img, ":"); i >= 0 {
		img = img[:i]
	}
	return img
}

func tipoPelaImagem(img string) string {
	switch baseImagem(img) {
	case "postgres", "postgis", "pgvector":
		return "postgres"
	case "mysql":
		return "mysql"
	case "mariadb":
		return "mariadb"
	case "mongo":
		return "mongo"
	case "redis", "valkey":
		return "redis"
	}
	return "app"
}

// --- serviços Compose do Easypanel -----------------------------------------

var composeDirRE = regexp.MustCompile(`^/etc/easypanel/projects/([^/]+)/([^/]+)/code(/.*)?$`)

// coletarCompose acha os serviços do tipo Compose: rodam com docker compose
// (fora do Swarm) a partir de /etc/easypanel/projects/<p>/<s>/code.
func coletarCompose(ctx context.Context, d Docker, pegar func(string) *Projeto, inv *Inventario) error {
	lines, err := d.Lines(ctx, "ps", "-a", "--filter", "label=com.docker.compose.project", "--format", `{{.Label "com.docker.compose.project.working_dir"}}`)
	if err != nil {
		return err
	}
	vistos := map[string]bool{}
	for _, dir := range lines {
		m := composeDirRE.FindStringSubmatch(dir)
		if m == nil || vistos[m[1]+"/"+m[2]] {
			continue
		}
		vistos[m[1]+"/"+m[2]] = true
		s := Servico{Nome: m[2], Slug: Slugify(m[2]), Tipo: "compose", Replicas: 1}
		files, err := d.ReadHostDir(ctx, dir, 1<<20)
		if err != nil {
			inv.Avisos = append(inv.Avisos, fmt.Sprintf("compose %s/%s: %v", m[1], m[2], err))
		}
		s.ComposeArquivos = map[string]string{}
		for name, body := range files {
			if strings.Contains(name, "/") {
				continue
			}
			if strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml") || name == ".env" {
				s.ComposeArquivos[name] = body
			}
		}
		p := pegar(m[1])
		p.Servicos = append(p.Servicos, s)
	}
	return nil
}

// --- Traefik ----------------------------------------------------------------

type rota struct {
	Host, Caminho string
	Porta         int
}

var (
	hostRE   = regexp.MustCompile("Host\\(([^)]*)\\)")
	prefixRE = regexp.MustCompile("PathPrefix\\(`([^`]*)`\\)")
	quotedRE = regexp.MustCompile("[`\"]([^`\"]+)[`\"]")
)

// lerTraefik devolve as rotas por destino ("projeto_serviço"), a partir da
// configuração dinâmica (YAML ou JSON) que o Easypanel gera.
func lerTraefik(files map[string]string) map[string][]rota {
	out := map[string][]rota{}
	visto := map[string]bool{}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		if !(strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".json")) {
			continue
		}
		var cfg struct {
			HTTP struct {
				Routers map[string]struct {
					Rule    string `yaml:"rule"`
					Service string `yaml:"service"`
				} `yaml:"routers"`
				Services map[string]struct {
					LoadBalancer struct {
						Servers []struct {
							URL string `yaml:"url"`
						} `yaml:"servers"`
					} `yaml:"loadBalancer"`
				} `yaml:"services"`
			} `yaml:"http"`
		}
		if err := yaml.Unmarshal([]byte(files[name]), &cfg); err != nil {
			continue
		}
		for _, r := range cfg.HTTP.Routers {
			svc, ok := cfg.HTTP.Services[r.Service]
			if !ok || len(svc.LoadBalancer.Servers) == 0 {
				continue
			}
			u, err := url.Parse(svc.LoadBalancer.Servers[0].URL)
			if err != nil || u.Hostname() == "" {
				continue
			}
			porta, _ := strconv.Atoi(u.Port())
			caminho := ""
			if m := prefixRE.FindStringSubmatch(r.Rule); m != nil && m[1] != "/" {
				caminho = m[1]
			}
			for _, hm := range hostRE.FindAllStringSubmatch(r.Rule, -1) {
				for _, q := range quotedRE.FindAllStringSubmatch(hm[1], -1) {
					host := strings.ToLower(q[1])
					chave := u.Hostname() + "|" + host + "|" + caminho
					if visto[chave] {
						continue // o Easypanel cria um router http e um https por domínio
					}
					visto[chave] = true
					out[u.Hostname()] = append(out[u.Hostname()], rota{Host: host, Caminho: caminho, Porta: porta})
				}
			}
		}
	}
	for k := range out {
		sort.Slice(out[k], func(i, j int) bool { return out[k][i].Host+out[k][i].Caminho < out[k][j].Host+out[k][j].Caminho })
	}
	return out
}

// --- conferências no servidor ----------------------------------------------

// marcarArquivos descobre, num container só, quais binds são arquivos (File
// Mount do Easypanel) e quais caminhos sumiram.
func marcarArquivos(ctx context.Context, d Docker, projetos map[string]*Projeto) error {
	var caminhos []string
	for _, p := range projetos {
		for _, s := range p.Servicos {
			for _, m := range s.Montagens {
				if m.Tipo == "bind" {
					caminhos = append(caminhos, m.Origem)
				}
			}
		}
	}
	if len(caminhos) == 0 {
		return nil
	}
	args := []string{"run", "--rm", "--network", "none", "--mount", "type=bind,src=/,dst=/host,readonly", d.Auxiliar,
		"sh", "-c", `for p; do if [ -d "/host$p" ]; then echo D; elif [ -e "/host$p" ]; then echo F; else echo X; fi; done`, "sh"}
	lines, err := d.Lines(ctx, append(args, caminhos...)...)
	if err != nil {
		return err
	}
	if len(lines) != len(caminhos) {
		return fmt.Errorf("resposta inesperada (%d de %d)", len(lines), len(caminhos))
	}
	tipo := map[string]string{}
	for i, c := range caminhos {
		tipo[c] = lines[i]
	}
	for _, p := range projetos {
		for i := range p.Servicos {
			for j := range p.Servicos[i].Montagens {
				m := &p.Servicos[i].Montagens[j]
				if m.Tipo != "bind" {
					continue
				}
				m.Arquivo = tipo[m.Origem] == "F"
				m.Ausente = tipo[m.Origem] == "X"
			}
		}
	}
	return nil
}

func portasEmUso(ctx context.Context, d Docker) bool {
	out, err := d.Run(ctx, "run", "--rm", "--network", "host", d.Auxiliar, "sh", "-c", "netstat -ltn 2>/dev/null || cat /proc/net/tcp /proc/net/tcp6")
	if err != nil {
		return false
	}
	s := string(out)
	// netstat: ":80 " / ":443 "; /proc/net/tcp: porta em hexa (0050, 01BB) com estado 0A (LISTEN).
	return strings.Contains(s, ":80 ") || strings.Contains(s, ":443 ") ||
		regexp.MustCompile(`:(0050|01BB) [0-9A-F:]+ 0A`).MatchString(s)
}

// JSON é o inventário como vai para o arquivo.
func (inv *Inventario) JSON() ([]byte, error) {
	return json.MarshalIndent(inv, "", "  ")
}
