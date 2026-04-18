package translate

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"syscall"

	"github.com/adrg/xdg"
	llama "github.com/tcpipuk/llama-go"
)

func init() {
	os.Setenv("LLAMA_LOG", "none")
	llama.InitLogging()
}

// SuppressStderr redirects file descriptor 2 to /dev/null permanently.
// Call this after the TUI is initialized so llama.cpp C-level output
// (Metal shader compilation, etc.) doesn't corrupt the terminal.
func SuppressStderr() {
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return
	}
	syscall.Dup2(int(devNull.Fd()), 2)
	os.Stderr = devNull
}

const (
	defaultModelFile = "translategemma-12b-it-Q4_K_M.gguf"
	modelDownloadURL = "https://huggingface.co/bullerwins/translategemma-12b-it-GGUF/resolve/main/translategemma-12b-it-Q4_K_M.gguf"
)

type ProgressFunc func(downloaded, total int64)

func GetModelDir() string {
	dir, err := xdg.DataFile("whatscli")
	if err != nil {
		return filepath.Join(os.TempDir(), "whatscli")
	}
	return filepath.Dir(dir)
}

func GetDefaultModelPath() string {
	return filepath.Join(GetModelDir(), defaultModelFile)
}

func ModelExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Size() > 0
}

func DownloadModel(path string, progress ProgressFunc) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create model directory: %w", err)
	}

	tmpPath := path + ".download"
	out, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer func() {
		out.Close()
		os.Remove(tmpPath)
	}()

	resp, err := http.Get(modelDownloadURL)
	if err != nil {
		return fmt.Errorf("download model: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download model: HTTP %d", resp.StatusCode)
	}

	total := resp.ContentLength
	var downloaded int64
	buf := make([]byte, 256*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := out.Write(buf[:n]); writeErr != nil {
				return fmt.Errorf("write model file: %w", writeErr)
			}
			downloaded += int64(n)
			if progress != nil {
				progress(downloaded, total)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fmt.Errorf("read model data: %w", readErr)
		}
	}

	out.Close()
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("finalize model file: %w", err)
	}
	return nil
}

func loadModel(path string) (*llama.Model, error) {
	model, err := llama.LoadModel(
		path,
		llama.WithGPULayers(-1),
		llama.WithSilentLoading(),
	)
	if err != nil {
		return nil, fmt.Errorf("load model: %w", err)
	}
	return model, nil
}

func newContext(model *llama.Model) (*llama.Context, error) {
	ctx, err := model.NewContext(
		llama.WithContext(2048),
	)
	if err != nil {
		return nil, fmt.Errorf("create context: %w", err)
	}
	return ctx, nil
}
