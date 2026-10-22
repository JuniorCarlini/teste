package main

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Pacote em disco:
//
//	inventario.json                 tudo o que foi lido do Easypanel
//	LEIA-ME.md                      passo a passo da importação
//	projetos/<p>/compose.yaml       para colar na aba Compose do Hangar
//	projetos/<p>/MIGRACAO.md        domínios, portas, Git e pendências
//	projetos/<p>/compose/<s>/       arquivos de serviços Compose do Easypanel
//	dados/<p>/<s>/<banco>.dump      PostgreSQL (pg_dump -Fc)
//	dados/<p>/<s>/mysql.sql.gz      MySQL/MariaDB (todas as bases da app)
//	dados/<p>/<s>/mongo.archive.gz  MongoDB (mongodump --archive --gzip)
//	dados/<p>/<s>/disco-<d>.tar.gz  conteúdo de cada disco
//	dados/<p>/<s>/arquivos/         File Mounts do Easypanel
//	imagens/<p>-<s>.tar.gz          imagens construídas no Easypanel (modo externo)
//	parados.txt                     serviços que o programa parou no Easypanel

type OpcoesExportar struct {
	Saida     string
	SemDados  bool
	Imagens   bool // docker save das imagens construídas (modo externo)
	PararApps bool
	Progresso func(string)
}

func Exportar(ctx context.Context, d Docker, inv *Inventario, op OpcoesExportar) error {
	log := op.Progresso
	if err := os.MkdirAll(op.Saida, 0o700); err != nil {
		return err
	}

	globais := map[string]bool{}
	for _, p := range inv.Projetos {
		for _, s := range p.Servicos {
			globais[p.Nome+"_"+s.Nome] = true
		}
	}
	planos := make([]*Plano, 0, len(inv.Projetos))
	for i := range inv.Projetos {
		pl, err := GerarPlano(&inv.Projetos[i], globais)
		if err != nil {
			return fmt.Errorf("projeto %s: %w", inv.Projetos[i].Nome, err)
		}
		planos = append(planos, pl)
	}

	var parados []string
	if op.PararApps && !op.SemDados {
		for _, p := range inv.Projetos {
			for _, s := range p.Servicos {
				if s.Tipo == "app" && s.SwarmNome != "" && s.Replicas > 0 {
					log(fmt.Sprintf("parando %s no Easypanel", s.SwarmNome))
					if _, err := d.Run(ctx, "service", "scale", "-d", fmt.Sprintf("%s=0", s.SwarmNome)); err != nil {
						return err
					}
					parados = append(parados, fmt.Sprintf("docker service scale %s=%d", s.SwarmNome, s.Replicas))
				}
			}
		}
		if len(parados) > 0 {
			if err := os.WriteFile(filepath.Join(op.Saida, "parados.txt"), []byte("# Para religar no Easypanel:\n"+strings.Join(parados, "\n")+"\n"), 0o600); err != nil {
				return err
			}
			// Os containers levam alguns segundos para sair.
			for _, p := range inv.Projetos {
				for _, s := range p.Servicos {
					if s.Tipo == "app" && s.SwarmNome != "" {
						_, _ = d.Run(ctx, "service", "scale", fmt.Sprintf("%s=0", s.SwarmNome))
					}
				}
			}
		}
	}

	for i, pl := range planos {
		p := &inv.Projetos[i]
		dir := filepath.Join(op.Saida, "projetos", p.Slug)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if pl.Compose != nil {
			if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), pl.Compose, 0o600); err != nil {
				return err
			}
		}
		for _, s := range p.Servicos {
			for nome, corpo := range s.ComposeArquivos {
				cd := filepath.Join(dir, "compose", s.Slug)
				if err := os.MkdirAll(cd, 0o700); err != nil {
					return err
				}
				if err := os.WriteFile(filepath.Join(cd, filepath.Base(nome)), []byte(corpo), 0o600); err != nil {
					return err
				}
			}
		}
		if !op.SemDados {
			for j := range p.Servicos {
				if err := exportarServico(ctx, d, op, p, &p.Servicos[j], log); err != nil {
					return fmt.Errorf("%s/%s: %w", p.Nome, p.Servicos[j].Nome, err)
				}
			}
		}
		if err := os.WriteFile(filepath.Join(dir, "MIGRACAO.md"), []byte(textoMigracao(pl, op)), 0o600); err != nil {
			return err
		}
	}

	b, err := inv.JSON()
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(op.Saida, "inventario.json"), b, 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(op.Saida, "LEIA-ME.md"), []byte(textoLeiaMe(inv, planos, op, parados)), 0o600)
}

// containerDoServico acha o container em execução de um serviço do Swarm.
func containerDoServico(ctx context.Context, d Docker, swarm string) (string, error) {
	ids, err := d.Lines(ctx, "ps", "-q", "--filter", "label=com.docker.swarm.service.name="+swarm)
	if err != nil {
		return "", err
	}
	if len(ids) == 0 {
		return "", fmt.Errorf("nenhum container de %s rodando (o banco precisa estar ligado para o dump)", swarm)
	}
	return ids[0], nil
}

func exportarServico(ctx context.Context, d Docker, op OpcoesExportar, p *Projeto, s *Servico, log func(string)) error {
	if s.Tipo == "compose" {
		return nil
	}
	dir := filepath.Join(op.Saida, "dados", p.Slug, s.Slug)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tam := func(n int64) string { return tamanho(n) }

	switch s.Tipo {
	case "postgres":
		ctr, err := containerDoServico(ctx, d, s.SwarmNome)
		if err != nil {
			return err
		}
		bancos, err := d.Lines(ctx, "exec", ctr, "sh", "-c",
			`psql -U "$POSTGRES_USER" -d postgres -Atc "select datname from pg_database where not datistemplate order by 1"`)
		if err != nil {
			return err
		}
		principal := BancoPrincipal(s)
		for _, b := range bancos {
			if b == "postgres" && principal != "postgres" {
				continue // banco de manutenção, vazio
			}
			log(fmt.Sprintf("dump %s/%s: banco %s", p.Nome, s.Nome, b))
			n, err := d.ToFile(ctx, filepath.Join(dir, b+".dump"), false, "exec", ctr, "sh", "-c", `exec pg_dump -Fc -U "$POSTGRES_USER" -d "$1"`, "sh", b)
			if err != nil {
				return err
			}
			log("  " + tam(n))
		}
		return nil
	case "mysql", "mariadb":
		ctr, err := containerDoServico(ctx, d, s.SwarmNome)
		if err != nil {
			return err
		}
		log(fmt.Sprintf("dump %s/%s (%s)", p.Nome, s.Nome, s.Tipo))
		n, err := d.ToFile(ctx, filepath.Join(dir, "mysql.sql.gz"), true, "exec", ctr, "sh", "-c", scriptMysqlDump)
		if err != nil {
			return err
		}
		log("  " + tam(n))
		return nil
	case "mongo":
		ctr, err := containerDoServico(ctx, d, s.SwarmNome)
		if err != nil {
			return err
		}
		log(fmt.Sprintf("dump %s/%s (mongo)", p.Nome, s.Nome))
		n, err := d.ToFile(ctx, filepath.Join(dir, "mongo.archive.gz"), false, "exec", ctr, "sh", "-c", scriptMongoDump)
		if err != nil {
			return err
		}
		log("  " + tam(n))
		return nil
	case "redis":
		// Grava o que está em memória antes de copiar /data.
		if ctr, err := containerDoServico(ctx, d, s.SwarmNome); err == nil {
			_, _ = d.Run(ctx, "exec", ctr, "sh", "-c", `redis-cli $(test -n "$1" && echo -a "$1" --no-auth-warning) SAVE`, "sh", senhaRedis(s.Comando))
		}
	}

	if s.ImagemLocal {
		destino := ImagemMigracao(p, s)
		log(fmt.Sprintf("imagem %s → %s", s.Imagem, destino))
		if _, err := d.Run(ctx, "tag", s.Imagem, destino); err != nil {
			return err
		}
		if op.Imagens {
			if err := os.MkdirAll(filepath.Join(op.Saida, "imagens"), 0o700); err != nil {
				return err
			}
			n, err := d.ToFile(ctx, filepath.Join(op.Saida, "imagens", p.Slug+"-"+s.Slug+".tar.gz"), true, "save", destino)
			if err != nil {
				return err
			}
			log("  " + tam(n))
		}
	}

	for _, m := range s.Montagens {
		if m.Arquivo && !m.Ausente {
			ad := filepath.Join(dir, "arquivos")
			if err := os.MkdirAll(ad, 0o700); err != nil {
				return err
			}
			if _, err := d.ToFile(ctx, filepath.Join(ad, strings.TrimPrefix(strings.ReplaceAll(path.Clean(m.Destino), "/", "_"), "_")), false,
				"run", "--rm", "--network", "none", "--mount", "type=bind,src="+m.Origem+",dst=/origem,readonly", d.Auxiliar, "cat", "/origem"); err != nil {
				return err
			}
			continue
		}
		if m.Disco == "" {
			continue
		}
		log(fmt.Sprintf("disco %s/%s: %s → %s", p.Nome, s.Nome, m.Origem, m.Disco))
		mount := "type=volume,src=" + m.Origem + ",dst=/origem,readonly,volume-nocopy"
		if m.Tipo == "bind" {
			mount = "type=bind,src=" + m.Origem + ",dst=/origem,readonly"
		}
		n, err := d.ToFile(ctx, filepath.Join(dir, "disco-"+m.Disco+".tar.gz"), false,
			"run", "--rm", "--network", "none", "--mount", mount, d.Auxiliar, "tar", "-C", "/origem", "-czf", "-", ".")
		if err != nil {
			return err
		}
		log("  " + tam(n))
	}
	return nil
}

// BancoPrincipal é o banco que a imagem do PostgreSQL cria (POSTGRES_DB, ou
// o nome do usuário).
func BancoPrincipal(s *Servico) string {
	env := s.EnvMap()
	if v := env["POSTGRES_DB"]; v != "" {
		return v
	}
	if v := env["POSTGRES_USER"]; v != "" {
		return v
	}
	return "postgres"
}

// Bases da aplicação (sem as do sistema), com rotinas, gatilhos e eventos.
// Funciona nas imagens do MySQL e do MariaDB (mariadb-dump nas novas).
const scriptMysqlDump = `P="${MARIADB_ROOT_PASSWORD:-$MYSQL_ROOT_PASSWORD}"
C=mysql; D=mysqldump
if command -v mariadb >/dev/null 2>&1; then C=mariadb; D=mariadb-dump; fi
DBS=$($C -uroot -p"$P" -N -e 'show databases' | grep -Ev '^(information_schema|performance_schema|mysql|sys)$' | tr '\n' ' ')
[ -n "$DBS" ] || exit 0
exec $D -uroot -p"$P" --single-transaction --routines --triggers --events --databases $DBS`

const scriptMongoDump = `if [ -n "$MONGO_INITDB_ROOT_USERNAME" ]; then
  exec mongodump --quiet --archive --gzip -u "$MONGO_INITDB_ROOT_USERNAME" -p "$MONGO_INITDB_ROOT_PASSWORD" --authenticationDatabase admin
fi
exec mongodump --quiet --archive --gzip`

// senhaRedis lê o --requirepass do comando do Easypanel.
func senhaRedis(cmd []string) string {
	for i, a := range cmd {
		if a == "--requirepass" && i+1 < len(cmd) {
			return cmd[i+1]
		}
		if strings.Contains(a, "--requirepass ") {
			f := strings.Fields(a[strings.Index(a, "--requirepass ")+len("--requirepass "):])
			if len(f) > 0 {
				return strings.Trim(f[0], `"'`)
			}
		}
	}
	return ""
}

func tamanho(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
}

func lista(titulo string, itens []string, vazio string) string {
	var b strings.Builder
	b.WriteString("## " + titulo + "\n\n")
	if len(itens) == 0 {
		b.WriteString(vazio + "\n\n")
		return b.String()
	}
	sort.Strings(itens)
	for _, i := range itens {
		b.WriteString("- [ ] " + i + "\n")
	}
	b.WriteString("\n")
	return b.String()
}

func textoMigracao(pl *Plano, op OpcoesExportar) string {
	p := pl.Projeto
	var b strings.Builder
	fmt.Fprintf(&b, "# Projeto %s\n\nNo Hangar: crie o projeto `%s`, abra **Novo serviço › Compose**, cole `compose.yaml`, **Analisar** e **Criar**.\n\n", p.Nome, p.Slug)
	b.WriteString("| Serviço | Tipo | Imagem | Discos |\n|---|---|---|---|\n")
	for _, s := range p.Servicos {
		var discos []string
		for _, m := range s.Montagens {
			if m.Disco != "" {
				discos = append(discos, m.Disco+" → "+m.Destino)
			}
		}
		img := s.Imagem
		if s.ImagemLocal {
			img = ImagemMigracao(p, &s) + " (construída no Easypanel)"
		}
		fmt.Fprintf(&b, "| %s | %s | `%s` | %s |\n", s.Nome, s.Tipo, img, strings.Join(discos, ", "))
	}
	b.WriteString("\n")
	if op.SemDados {
		b.WriteString("> Exportado **sem dados** (`--sem-dados`): só a configuração.\n\n")
	} else {
		b.WriteString(lista("Dados que `restaurar` devolve", pl.Restauracao, "Nenhum."))
	}
	b.WriteString(lista("Domínios (cadastrar no serviço, aba Domínios)", pl.Dominios, "Nenhum domínio encontrado no Traefik do Easypanel."))
	b.WriteString(lista("Portas publicadas no Easypanel (abrir na aba Rede, se ainda precisar)", pl.Portas, "Nenhuma."))
	b.WriteString(lista("Apps construídas no Easypanel (depois, trocar a origem para Git no Hangar)", pl.Git, "Nenhuma."))
	b.WriteString(lista("Pendências", pl.Pendencias, "Nenhuma."))
	return b.String()
}

func textoLeiaMe(inv *Inventario, planos []*Plano, op OpcoesExportar, parados []string) string {
	var b strings.Builder
	b.WriteString("# Exportação do Easypanel")
	if inv.EasypanelVers != "" {
		b.WriteString(" " + inv.EasypanelVers)
	}
	fmt.Fprintf(&b, "\n\nServidor: `%s` · Docker %s · gerado em %s UTC\n\n", inv.Host, inv.DockerVersao, inv.GeradoEm.Format("2006-01-02 15:04"))
	if len(inv.Avisos) > 0 {
		b.WriteString("## Avisos\n\n")
		for _, a := range inv.Avisos {
			b.WriteString("- " + a + "\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("## Projetos\n\n")
	for _, pl := range planos {
		fmt.Fprintf(&b, "- `%s` → [projetos/%s/MIGRACAO.md](projetos/%s/MIGRACAO.md) (%d serviços, %d pendências)\n", pl.Projeto.Nome, pl.Projeto.Slug, pl.Projeto.Slug, len(pl.Projeto.Servicos), len(pl.Pendencias))
	}
	b.WriteString(`
## Ordem da importação

1. Hangar instalado e com a conta de dono criada.
2. ` + "`easypanel-para-hangar preparar --pacote .`" + ` no servidor do Hangar: carrega as imagens construídas no Easypanel (modo externo).
3. Para cada projeto: criar o projeto no Hangar com o mesmo nome, colar o ` + "`compose.yaml`" + ` na aba Compose, Analisar, Criar, e esperar o deploy.
4. ` + "`easypanel-para-hangar restaurar --pacote .`" + ` no servidor do Hangar: dumps dos bancos e conteúdo dos discos.
5. Seguir o MIGRACAO.md de cada projeto: domínios, portas, Git, pendências.
6. Apontar o DNS (ou, na mesma VPS, trocar o Traefik do Easypanel pelo Caddy do Hangar).
`)
	if len(parados) > 0 {
		b.WriteString("\n## Serviços parados no Easypanel\n\nPara religar (voltar atrás):\n\n```sh\n" + strings.Join(parados, "\n") + "\n```\n")
	}
	return b.String()
}
