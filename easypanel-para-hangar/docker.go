package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"strings"
)

// Docker roda o CLI `docker`. Com Host vazio fala com o Docker local; com
// Host = "ssh://root@vps" fala com o Docker da VPS por SSH (o próprio CLI do
// Docker faz o túnel), então nada precisa ser instalado na VPS.
type Docker struct {
	Host string
	// Auxiliar é a imagem usada para ler volumes e pastas do servidor.
	Auxiliar string
}

func (d Docker) cmd(ctx context.Context, args ...string) *exec.Cmd {
	c := exec.CommandContext(ctx, "docker", args...) //nolint:gosec // argumentos montados pelo programa
	c.Env = os.Environ()
	if d.Host != "" {
		c.Env = append(c.Env, "DOCKER_HOST="+d.Host)
	}
	return c
}

// Run roda um comando e devolve a saída padrão.
func (d Docker) Run(ctx context.Context, args ...string) ([]byte, error) {
	c := d.cmd(ctx, args...)
	var stderr bytes.Buffer
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		return out, fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// JSON roda um comando e decodifica a saída.
func (d Docker) JSON(ctx context.Context, v any, args ...string) error {
	out, err := d.Run(ctx, args...)
	if err != nil {
		return err
	}
	return json.Unmarshal(out, v)
}

// Lines roda um comando e devolve as linhas não vazias.
func (d Docker) Lines(ctx context.Context, args ...string) ([]string, error) {
	out, err := d.Run(ctx, args...)
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return lines, nil
}

// ToFile roda um comando e grava a saída padrão em arquivo (dumps, tars),
// comprimindo com gzip se gz. O arquivo só fica com o nome final se o comando
// terminar bem.
func (d Docker) ToFile(ctx context.Context, file string, gz bool, args ...string) (int64, error) {
	tmp := file + ".parcial"
	f, err := os.Create(tmp) //nolint:gosec // caminho dentro do pacote escolhido pelo operador
	if err != nil {
		return 0, err
	}
	c := d.cmd(ctx, args...)
	var stderr bytes.Buffer
	var zw *gzip.Writer
	c.Stdout, c.Stderr = f, &stderr
	if gz {
		zw = gzip.NewWriter(f)
		c.Stdout = zw
	}
	runErr := c.Run()
	if zw != nil {
		if err := zw.Close(); err != nil && runErr == nil {
			runErr = err
		}
	}
	closeErr := f.Close()
	if runErr != nil {
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), runErr, lastLines(stderr.String(), 5))
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return 0, closeErr
	}
	st, err := os.Stat(tmp)
	if err != nil {
		return 0, err
	}
	return st.Size(), os.Rename(tmp, file)
}

// FromFile roda um comando com o arquivo na entrada padrão (restaurações),
// descomprimindo com gzip se gunzip.
func (d Docker) FromFile(ctx context.Context, file string, gunzip bool, args ...string) error {
	f, err := os.Open(file) //nolint:gosec // arquivo do pacote
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	var in io.Reader = f
	if gunzip {
		zr, err := gzip.NewReader(f)
		if err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		in = zr
	}
	c := d.cmd(ctx, args...)
	var stderr bytes.Buffer
	c.Stdin, c.Stdout, c.Stderr = in, io.Discard, &stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, lastLines(stderr.String(), 5))
	}
	return nil
}

// ReadHostDir lê os arquivos (até maxBytes cada) de uma pasta do servidor,
// montando-a só para leitura num container auxiliar. Funciona igual no Docker
// local e no remoto. Pasta inexistente devolve mapa vazio.
func (d Docker) ReadHostDir(ctx context.Context, dir string, maxBytes int64) (map[string]string, error) {
	// --mount (e não -v): com -v o Docker criaria a pasta no servidor.
	out, err := d.Run(ctx, "run", "--rm", "--network", "none", "--mount", "type=bind,src="+dir+",dst=/origem,readonly", d.Auxiliar,
		"tar", "-C", "/origem", "-cf", "-", ".")
	if err != nil {
		if strings.Contains(err.Error(), "does not exist") {
			return map[string]string{}, nil
		}
		return nil, err
	}
	files := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(out))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return files, nil // pasta vazia ou inexistente: tar vazio
		}
		if h.Typeflag != tar.TypeReg || h.Size > maxBytes {
			continue
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		files[path.Clean(h.Name)] = string(b)
	}
	return files, nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}
