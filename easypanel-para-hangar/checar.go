package main

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Checar é a conferência antes de instalar o Hangar na mesma VPS do
// Easypanel. O ponto crítico é o Docker: sem /etc/docker/daemon.json, o
// instalador do Hangar escreve um com "live-restore": true e reinicia o
// Docker, e o Docker não sobe com live-restore em modo Swarm, derrubando o
// Easypanel e tudo o que ele roda.
func Checar(ctx context.Context, d Docker, inv *Inventario, w io.Writer) (criticos int) {
	ok := func(s string) { fmt.Fprintln(w, "  ok      "+s) }
	aviso := func(s string) { fmt.Fprintln(w, "  aviso   "+s) }
	critico := func(s string) { criticos++; fmt.Fprintln(w, "  CRÍTICO "+s) }

	fmt.Fprintf(w, "Servidor %s (Docker %s)\n\n", inv.Host, inv.DockerVersao)

	if inv.Swarm {
		ok("Docker em modo Swarm (Easypanel)")
	} else {
		aviso("Docker fora do modo Swarm: não parece um servidor com Easypanel")
	}

	daemon, _ := d.Run(ctx, "run", "--rm", "--network", "none", "--mount", "type=bind,src=/etc/docker,dst=/d,readonly", d.Auxiliar, "sh", "-c", "cat /d/daemon.json 2>/dev/null || true")
	conteudo := strings.ReplaceAll(string(daemon), " ", "")
	switch {
	case inv.Swarm && strings.Contains(conteudo, `"live-restore":true`):
		critico("/etc/docker/daemon.json tem live-restore ligado com Swarm ativo: o Docker não reinicia assim")
	case inv.Swarm && strings.TrimSpace(string(daemon)) == "":
		critico(`/etc/docker/daemon.json não existe: o instalador do Hangar vai criar um com "live-restore": true
           e reiniciar o Docker, que NÃO sobe com Swarm ativo (o Easypanel cai). Antes de instalar, crie:
             echo '{"log-driver":"local","log-opts":{"max-size":"50m","max-file":"3"}}' | sudo tee /etc/docker/daemon.json
             sudo systemctl restart docker
           (o instalador só escreve o arquivo quando ele não existe)`)
	default:
		ok("/etc/docker/daemon.json existe e não liga live-restore")
	}

	if inv.Portas80e443 {
		aviso("portas 80/443 em uso (Traefik do Easypanel): o Hangar instala sem o proxy dele e o painel fica\n           só pelo túnel SSH (ssh -L 7700:127.0.0.1:7700 ...). Na virada, pare o Traefik e ligue [edge] no hangar.toml.")
	} else {
		ok("portas 80/443 livres")
	}

	if osr, err := d.Run(ctx, "run", "--rm", "--network", "none", "--mount", "type=bind,src=/etc,dst=/e,readonly", d.Auxiliar, "cat", "/e/os-release"); err == nil {
		id, ver := campoOS(string(osr), "ID"), campoOS(string(osr), "VERSION_ID")
		switch {
		case id == "debian" && versao(ver) >= 13, id == "ubuntu" && versao(ver) >= 24.04:
			ok(fmt.Sprintf("sistema %s %s (suportado pelo Hangar)", id, ver))
		default:
			aviso(fmt.Sprintf("sistema %s %s: o Hangar pede Debian 13 ou Ubuntu 24.04+; na mesma VPS isso pode não instalar", id, ver))
		}
	}

	if df, err := d.Run(ctx, "run", "--rm", "--network", "none", "--mount", "type=bind,src=/,dst=/host,readonly", d.Auxiliar, "df", "-h", "/host"); err == nil {
		linhas := strings.Split(strings.TrimSpace(string(df)), "\n")
		f := strings.Fields(linhas[len(linhas)-1])
		if len(f) >= 4 {
			ok(fmt.Sprintf("disco: %s livres de %s. A exportação + o Hangar ocupam ~2x os dados (bancos e discos); confira antes.", f[3], f[1]))
		}
	}

	if !inv.TraefikLido {
		aviso("configuração do Traefik do Easypanel não encontrada: os domínios não entram no relatório")
	}
	if !inv.APIUsada {
		aviso("sem --api: as apps construídas no Easypanel vão como imagem local, sem o repositório Git anotado")
	}

	var apps, bancos, compose int
	for _, p := range inv.Projetos {
		for _, s := range p.Servicos {
			switch {
			case s.Tipo == "compose":
				compose++
			case s.Banco():
				bancos++
			default:
				apps++
			}
		}
	}
	fmt.Fprintf(w, "\n%d projetos: %d apps, %d bancos, %d serviços Compose\n", len(inv.Projetos), apps, bancos, compose)
	for _, a := range inv.Avisos {
		fmt.Fprintln(w, "  aviso   "+a)
	}
	return criticos
}

func versao(v string) float64 {
	f, _ := strconv.ParseFloat(v, 64)
	return f
}

func campoOS(s, k string) string {
	for _, l := range strings.Split(s, "\n") {
		if v, ok := strings.CutPrefix(l, k+"="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return ""
}
