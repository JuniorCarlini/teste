package main

import (
	"strings"
	"testing"
)

func lojaDeTeste() *Projeto {
	return &Projeto{Nome: "loja", Slug: "loja", Servicos: []Servico{
		{Nome: "db", Slug: "db", Tipo: "postgres", Imagem: "postgres:16", Replicas: 1,
			Env:       []EnvVar{{"POSTGRES_DB", "loja"}, {"POSTGRES_USER", "app"}, {"POSTGRES_PASSWORD", "p$1"}, {"PGDATA", "/x"}},
			Montagens: []Montagem{{Tipo: "volume", Origem: "loja_db_data", Destino: "/var/lib/postgresql/data"}}},
		{Nome: "cache", Slug: "cache", Tipo: "redis", Imagem: "redis:7", Replicas: 1, Comando: []string{"redis-server", "--requirepass", "rpw"},
			Montagens: []Montagem{{Tipo: "volume", Origem: "v", Destino: "/data"}}},
		{Nome: "web_app", Slug: "web-app", Tipo: "app", Imagem: "easypanel/loja/web_app", ImagemLocal: true, Replicas: 2, PortaInterna: 3000, MemoriaMB: 512,
			Env: []EnvVar{{"DATABASE_URL", "postgres://app:p$1@loja_db:5432/loja"}, {"BLOG", "http://blog_wp"}},
			Montagens: []Montagem{
				{Tipo: "bind", Origem: "/etc/easypanel/projects/loja/web_app/volumes/media", Destino: "/app/media"},
				{Tipo: "bind", Origem: "/etc/easypanel/projects/loja/web_app/volumes/x", Destino: "/srv/media"},
				{Tipo: "bind", Origem: "/etc/easypanel/f/a.json", Destino: "/app/a.json", Arquivo: true},
				{Tipo: "bind", Origem: "/var/run/docker.sock", Destino: "/var/run/docker.sock"},
			},
			Dominios: []Dominio{{Host: "loja.com", Porta: 3000}},
			Git:      &FonteGit{Repo: "https://github.com/a/b", Branch: "main", Build: "nixpacks"}},
	}}
}

func TestGerarPlano(t *testing.T) {
	p := lojaDeTeste()
	pl, err := GerarPlano(p, map[string]bool{"loja_db": true, "loja_cache": true, "loja_web_app": true, "blog_wp": true})
	if err != nil {
		t.Fatal(err)
	}
	c := string(pl.Compose)
	for _, want := range []string{
		"image: ep2hangar.invalid/loja-web-app:migracao",
		"DATABASE_URL: postgres://app:p$$1@db:5432/loja", // host trocado, "$" escapado
		"POSTGRES_PASSWORD: p$$1",
		"- media:/app/media",
		"- media-2:/srv/media",
		`- "3000"`,
		"memory: 512M",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("compose sem %q:\n%s", want, c)
		}
	}
	if strings.Contains(c, "requirepass") || strings.Contains(c, "rpw") {
		t.Errorf("senha do Redis vazou no compose:\n%s", c)
	}
	if strings.Contains(c, "PGDATA") || strings.Contains(c, "docker.sock") || strings.Contains(c, "a.json") {
		t.Errorf("compose com o que devia ficar de fora:\n%s", c)
	}
	if got := p.Servicos[1].Montagens[0].Disco; got != "data" {
		t.Errorf("disco do Redis = %q, quero data (o do template)", got)
	}
	pend := strings.Join(pl.Pendencias, "\n")
	for _, want := range []string{"blog_wp", "File Mount", "socket do Docker", "2 réplicas"} {
		if !strings.Contains(pend, want) {
			t.Errorf("pendências sem %q:\n%s", want, pend)
		}
	}
	if len(pl.Git) != 1 || !strings.Contains(pl.Git[0], "nixpacks") {
		t.Errorf("git = %v", pl.Git)
	}
}

func TestGerarPlanoNomeRepetido(t *testing.T) {
	p := &Projeto{Nome: "x", Slug: "x", Servicos: []Servico{{Nome: "web_app", Slug: "web-app"}, {Nome: "web-app", Slug: "web-app"}}}
	if _, err := GerarPlano(p, nil); err == nil {
		t.Fatal("dois serviços com o mesmo endereço deviam falhar")
	}
}

func TestSlugify(t *testing.T) {
	for in, want := range map[string]string{"Loja Café": "loja-cafe", "web_app": "web-app", "1api": "s-1api", "_": ""} {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, quero %q", in, got, want)
		}
	}
}

func TestNomeDisco(t *testing.T) {
	usados := map[string]bool{}
	for _, c := range []struct{ in, want string }{{"/app/media", "media"}, {"/x/media", "media-2"}, {"/data/1", "s-1"}, {"/app/Uploads_Old", "uploads-old"}} {
		got := nomeDisco(c.in, usados)
		usados[got] = true
		if got != c.want {
			t.Errorf("nomeDisco(%q) = %q, quero %q", c.in, got, c.want)
		}
	}
}

func TestLerTraefik(t *testing.T) {
	cfg := "http:\n  routers:\n    a: {rule: \"Host(`Loja.com`) && PathPrefix(`/`)\", service: s0}\n    b: {rule: \"Host(`loja.com`)\", service: s0}\n" +
		"    c: {rule: \"Host(`api.loja.com`) && PathPrefix(`/v1`)\", service: s1}\n" +
		"  services:\n    s0: {loadBalancer: {servers: [{url: 'http://loja_web:3000/'}]}}\n    s1: {loadBalancer: {servers: [{url: 'http://loja_api:80'}]}}\n"
	r := lerTraefik(map[string]string{"main.yaml": cfg, "x.txt": "lixo"})
	if len(r["loja_web"]) != 1 || r["loja_web"][0] != (rota{Host: "loja.com", Porta: 3000}) {
		t.Errorf("loja_web = %+v", r["loja_web"])
	}
	if len(r["loja_api"]) != 1 || r["loja_api"][0] != (rota{Host: "api.loja.com", Caminho: "/v1", Porta: 80}) {
		t.Errorf("loja_api = %+v", r["loja_api"])
	}
}

func TestSepararNome(t *testing.T) {
	if _, _, ok := separarNome("easypanel-traefik", nil); ok {
		t.Error("serviço do Easypanel não deve entrar")
	}
	if p, s, _ := separarNome("loja_web_app", nil); p != "loja" || s != "web_app" {
		t.Errorf("sem API: %s / %s", p, s)
	}
	api := &apiDados{servicos: []apiServico{{projeto: "minha_loja", nome: "web"}}}
	if p, s, _ := separarNome("minha_loja_web", api); p != "minha_loja" || s != "web" {
		t.Errorf("com API: %s / %s", p, s)
	}
}

func TestExtrairServicosEFonteGit(t *testing.T) {
	v := map[string]any{"services": []any{map[string]any{
		"projectName": "loja", "name": "web", "type": "app",
		"source": map[string]any{"type": "github", "owner": "a", "repo": "b", "ref": "main", "path": "/api/"},
		"build":  map[string]any{"type": "dockerfile", "file": "Dockerfile.prod"},
	}}}
	ss := extrairServicos(v)
	if len(ss) != 1 {
		t.Fatalf("serviços = %v", ss)
	}
	g := fonteGit(ss[0].bruto)
	if g == nil || g.Repo != "https://github.com/a/b" || g.Branch != "main" || g.Pasta != "api" || g.Dockerfile != "Dockerfile.prod" {
		t.Errorf("git = %+v", g)
	}
}

func TestSenhaRedis(t *testing.T) {
	if s := senhaRedis([]string{"redis-server", "--requirepass", "abc"}); s != "abc" {
		t.Error(s)
	}
	if s := senhaRedis([]string{"sh", "-c", `redis-server --requirepass "xyz" --appendonly yes`}); s != "xyz" {
		t.Error(s)
	}
}
