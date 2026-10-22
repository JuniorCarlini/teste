// Comando easypanel-para-hangar: lê um servidor com Easypanel e leva os
// projetos (configuração e dados) para o Hangar. Ver README.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

const uso = `easypanel-para-hangar — leva os projetos do Easypanel para o Hangar

No servidor do EASYPANEL (ou de fora, com --host ssh://root@vps):
  checar      confere se dá para instalar o Hangar na mesma VPS
  inventario  mostra o que existe no Easypanel (projetos, serviços, discos, domínios)
  exportar    grava o pacote: compose.yaml por projeto, dumps, discos e relatório

No servidor do HANGAR (com o pacote):
  preparar    carrega as imagens construídas no Easypanel (antes de importar)
  restaurar   devolve os dados aos bancos e discos (depois de importar)

Opções comuns:
  --host ssh://root@vps   Docker de outro servidor, por SSH (padrão: o local)
  --api URL               painel do Easypanel (ex.: http://127.0.0.1:3000), para ler a origem Git
  --token TOKEN           token da API do Easypanel (ou EASYPANEL_TOKEN)
  --projeto NOME          só este projeto (repita para vários)
  --auxiliar IMAGEM       imagem para ler volumes e pastas (padrão alpine:3.22)

Exemplos:
  easypanel-para-hangar checar
  easypanel-para-hangar exportar --saida /root/migracao --api http://127.0.0.1:3000 --token ...
  easypanel-para-hangar exportar --host ssh://root@1.2.3.4 --saida ./migracao --imagens --parar-apps
  easypanel-para-hangar restaurar --pacote /root/migracao
`

type listaFlag []string

func (l *listaFlag) String() string     { return strings.Join(*l, ",") }
func (l *listaFlag) Set(v string) error { *l = append(*l, v); return nil }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := rodar(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "erro:", err)
		os.Exit(1)
	}
}

func rodar(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "ajuda" {
		fmt.Fprint(stdout, uso)
		return nil
	}
	cmd := args[0]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		host      = fs.String("host", "", "")
		apiURL    = fs.String("api", "", "")
		token     = fs.String("token", os.Getenv("EASYPANEL_TOKEN"), "")
		auxiliar  = fs.String("auxiliar", "alpine:3.22", "")
		saida     = fs.String("saida", "", "")
		pacote    = fs.String("pacote", "", "")
		semDados  = fs.Bool("sem-dados", false, "")
		imagens   = fs.Bool("imagens", false, "")
		pararApps = fs.Bool("parar-apps", false, "")
		limpar    = fs.Bool("limpar", false, "")
		jsonSaida = fs.Bool("json", false, "")
		projetos  listaFlag
		destinos  listaFlag
	)
	fs.Var(&projetos, "projeto", "")
	fs.Var(&destinos, "destino", "")
	fs.Usage = func() { fmt.Fprint(stderr, uso) }
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	d := Docker{Host: *host, Auxiliar: *auxiliar}
	log := func(s string) { fmt.Fprintln(stderr, s) }
	coletar := func() (*Inventario, error) {
		if *apiURL != "" && *token == "" {
			return nil, errors.New("--api precisa de --token (ou EASYPANEL_TOKEN)")
		}
		return Coletar(ctx, d, OpcoesInventario{APIURL: *apiURL, APIToken: *token, Projetos: projetos})
	}

	switch cmd {
	case "checar":
		inv, err := coletar()
		if err != nil {
			return err
		}
		if n := Checar(ctx, d, inv, stdout); n > 0 {
			return fmt.Errorf("%d problema(s) crítico(s): resolva antes de instalar o Hangar nesta VPS", n)
		}
		return nil

	case "inventario":
		inv, err := coletar()
		if err != nil {
			return err
		}
		if *jsonSaida {
			b, err := inv.JSON()
			if err != nil {
				return err
			}
			_, err = stdout.Write(append(b, '\n'))
			return err
		}
		imprimirInventario(stdout, inv)
		return nil

	case "exportar":
		if *saida == "" {
			return errors.New("informe --saida PASTA")
		}
		if ents, err := os.ReadDir(*saida); err == nil && len(ents) > 0 {
			return fmt.Errorf("a pasta %s não está vazia", *saida)
		}
		inv, err := coletar()
		if err != nil {
			return err
		}
		if len(inv.Projetos) == 0 {
			return errors.New("nenhum projeto encontrado")
		}
		if *imagens && *host == "" {
			log("aviso: --imagens no próprio servidor só é útil se o pacote for levado para outro lugar")
		}
		err = Exportar(ctx, d, inv, OpcoesExportar{Saida: *saida, SemDados: *semDados, Imagens: *imagens, PararApps: *pararApps, Progresso: log})
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Pacote pronto em %s. Comece pelo LEIA-ME.md.\n", *saida)
		return nil

	case "preparar":
		if *pacote == "" {
			return errors.New("informe --pacote PASTA")
		}
		return Preparar(ctx, d, *pacote, log)

	case "restaurar":
		if *pacote == "" {
			return errors.New("informe --pacote PASTA")
		}
		dest := map[string]string{}
		for _, v := range destinos {
			de, para, ok := strings.Cut(v, "=")
			if !ok || Slugify(para) != para {
				return fmt.Errorf("--destino %q: use projeto-do-easypanel=projeto-no-hangar", v)
			}
			dest[de] = para
		}
		if err := Restaurar(ctx, d, OpcoesRestaurar{Pacote: *pacote, Projetos: projetos, Destino: dest, Limpar: *limpar, Progresso: log}); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "Restauração concluída.")
		return nil
	}
	fmt.Fprint(stderr, uso)
	return fmt.Errorf("comando desconhecido: %s", cmd)
}

func imprimirInventario(w io.Writer, inv *Inventario) {
	fmt.Fprintf(w, "Servidor %s · Docker %s · Swarm %v · API %v\n", inv.Host, inv.DockerVersao, inv.Swarm, inv.APIUsada)
	for _, p := range inv.Projetos {
		fmt.Fprintf(w, "\n%s (no Hangar: %s)\n", p.Nome, p.Slug)
		for _, s := range p.Servicos {
			extra := []string{}
			if s.Imagem != "" {
				extra = append(extra, s.Imagem)
			}
			if s.Git != nil {
				extra = append(extra, "git "+s.Git.Repo)
			}
			for _, m := range s.Montagens {
				extra = append(extra, "disco "+m.Destino)
			}
			for _, d := range s.Dominios {
				extra = append(extra, "https://"+d.Host+d.Caminho)
			}
			if s.Replicas == 0 {
				extra = append(extra, "PARADO")
			}
			fmt.Fprintf(w, "  %-20s %-9s %s\n", s.Nome, s.Tipo, strings.Join(extra, " · "))
		}
	}
	for _, a := range inv.Avisos {
		fmt.Fprintln(w, "aviso:", a)
	}
}
