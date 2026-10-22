package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// A restauração roda no servidor do Hangar, depois que o compose.yaml de
// cada projeto foi importado e o deploy terminou: o Hangar já criou os
// containers e os discos (com as etiquetas dele), e aqui só se põe o
// conteúdo dentro. Nada no banco do Hangar é tocado.

type OpcoesRestaurar struct {
	Pacote    string
	Projetos  []string          // vazio = todos
	Destino   map[string]string // projeto do Easypanel → slug do projeto no Hangar
	Limpar    bool              // esvaziar o disco antes de copiar
	Progresso func(string)
}

func lerPacote(dir string) (*Inventario, error) {
	b, err := os.ReadFile(filepath.Join(dir, "inventario.json")) //nolint:gosec // pacote escolhido pelo operador
	if err != nil {
		return nil, fmt.Errorf("pacote inválido (sem inventario.json): %w", err)
	}
	var inv Inventario
	if err := json.Unmarshal(b, &inv); err != nil {
		return nil, err
	}
	return &inv, nil
}

// Preparar carrega no Docker do Hangar as imagens construídas no Easypanel
// (só existem no pacote do modo externo). Roda antes de importar o compose.
func Preparar(ctx context.Context, d Docker, pacote string, log func(string)) error {
	arqs, _ := filepath.Glob(filepath.Join(pacote, "imagens", "*.tar.gz"))
	if len(arqs) == 0 {
		log("nenhuma imagem no pacote (na mesma VPS elas já estão no Docker)")
		return nil
	}
	sort.Strings(arqs)
	for _, a := range arqs {
		log("carregando " + filepath.Base(a))
		if err := d.FromFile(ctx, a, true, "load"); err != nil {
			return err
		}
	}
	return nil
}

func Restaurar(ctx context.Context, d Docker, op OpcoesRestaurar) error {
	inv, err := lerPacote(op.Pacote)
	if err != nil {
		return err
	}
	log := op.Progresso
	filtro := map[string]bool{}
	for _, p := range op.Projetos {
		filtro[p] = true
	}
	var falhas []string
	for _, p := range inv.Projetos {
		if len(filtro) > 0 && !filtro[p.Nome] && !filtro[p.Slug] {
			continue
		}
		hp := p.Slug
		if v := op.Destino[p.Nome]; v != "" {
			hp = v
		}
		for _, s := range p.Servicos {
			if s.Tipo == "compose" {
				continue
			}
			dados := filepath.Join(op.Pacote, "dados", p.Slug, s.Slug)
			if _, err := os.Stat(dados); err != nil {
				continue
			}
			ctr := hp + "_" + s.Slug
			if err := restaurarServico(ctx, d, op, hp, ctr, &s, dados, log); err != nil {
				log("  ERRO: " + err.Error())
				falhas = append(falhas, fmt.Sprintf("%s/%s: %v", p.Nome, s.Nome, err))
			}
		}
	}
	if len(falhas) > 0 {
		return fmt.Errorf("%d restauração(ões) falharam:\n  %s", len(falhas), strings.Join(falhas, "\n  "))
	}
	return nil
}

func restaurarServico(ctx context.Context, d Docker, op OpcoesRestaurar, hp, ctr string, s *Servico, dados string, log func(string)) error {
	rodando := func() error {
		out, err := d.Run(ctx, "inspect", "-f", "{{.State.Running}}", ctr)
		if err != nil {
			return fmt.Errorf("container %s não existe: importe o compose.yaml no Hangar e espere o deploy", ctr)
		}
		if strings.TrimSpace(string(out)) != "true" {
			return fmt.Errorf("container %s está parado: ligue o serviço no Hangar", ctr)
		}
		return nil
	}

	switch s.Tipo {
	case "postgres":
		if err := rodando(); err != nil {
			return err
		}
		dumps, _ := filepath.Glob(filepath.Join(dados, "*.dump"))
		sort.Strings(dumps)
		for _, f := range dumps {
			banco := strings.TrimSuffix(filepath.Base(f), ".dump")
			log(fmt.Sprintf("%s: pg_restore do banco %s", ctr, banco))
			if err := d.FromFile(ctx, f, false, "exec", "-i", ctr, "sh", "-c", scriptPgRestore, "sh", banco); err != nil {
				return err
			}
		}
		return nil
	case "mysql", "mariadb":
		f := filepath.Join(dados, "mysql.sql.gz")
		if _, err := os.Stat(f); err != nil {
			return nil
		}
		if err := rodando(); err != nil {
			return err
		}
		log(fmt.Sprintf("%s: restaurando dump %s", ctr, s.Tipo))
		return d.FromFile(ctx, f, true, "exec", "-i", ctr, "sh", "-c", scriptMysqlRestore)
	case "mongo":
		f := filepath.Join(dados, "mongo.archive.gz")
		if _, err := os.Stat(f); err != nil {
			return nil
		}
		if err := rodando(); err != nil {
			return err
		}
		log(fmt.Sprintf("%s: mongorestore", ctr))
		return d.FromFile(ctx, f, false, "exec", "-i", ctr, "sh", "-c", scriptMongoRestore)
	}

	for _, m := range s.Montagens {
		if m.Disco == "" {
			continue
		}
		arq := filepath.Join(dados, "disco-"+m.Disco+".tar.gz")
		if _, err := os.Stat(arq); err != nil {
			continue
		}
		vol := "hangar_" + hp + "_" + s.Slug + "_" + m.Disco
		if _, err := d.Run(ctx, "volume", "inspect", vol); err != nil {
			return fmt.Errorf("disco %s não existe no Hangar: importe o compose.yaml e espere o deploy", vol)
		}
		// Copiar com a app rodando corromperia arquivos abertos (e o Redis
		// regravaria /data ao parar): para quem usa o disco, copia, religa.
		usando, err := d.Lines(ctx, "ps", "-q", "--filter", "volume="+vol)
		if err != nil {
			return err
		}
		if len(usando) > 0 {
			log(fmt.Sprintf("parando %d container(s) que usam %s", len(usando), vol))
			if _, err := d.Run(ctx, append([]string{"stop", "-t", "60"}, usando...)...); err != nil {
				return err
			}
		}
		script := "tar -C /destino -xzpf -"
		if op.Limpar {
			script = "find /destino -mindepth 1 -delete && " + script
		}
		log(fmt.Sprintf("disco %s ← %s", vol, filepath.Base(arq)))
		restErr := d.FromFile(ctx, arq, false, "run", "--rm", "-i", "--network", "none", "--mount", "type=volume,src="+vol+",dst=/destino", d.Auxiliar, "sh", "-c", script)
		if len(usando) > 0 {
			if _, err := d.Run(ctx, append([]string{"start"}, usando...)...); err != nil && restErr == nil {
				restErr = err
			}
		}
		if restErr != nil {
			return restErr
		}
	}
	return nil
}

// O banco principal o template já criou; os outros são criados aqui.
const scriptPgRestore = `createdb -U "$POSTGRES_USER" "$1" 2>/dev/null
exec pg_restore --clean --if-exists --no-owner --no-acl -U "$POSTGRES_USER" -d "$1"`

const scriptMysqlRestore = `P="${MARIADB_ROOT_PASSWORD:-$MYSQL_ROOT_PASSWORD}"
C=mysql
if command -v mariadb >/dev/null 2>&1; then C=mariadb; fi
exec $C -uroot -p"$P"`

const scriptMongoRestore = `if [ -n "$MONGO_INITDB_ROOT_USERNAME" ]; then
  exec mongorestore --quiet --archive --gzip --drop -u "$MONGO_INITDB_ROOT_USERNAME" -p "$MONGO_INITDB_ROOT_PASSWORD" --authenticationDatabase admin
fi
exec mongorestore --quiet --archive --gzip --drop`
