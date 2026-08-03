package main

import (
	"fmt"
	"net"
	"net/http"
	_ "net/http/pprof" // debug endpoint, only served when WHATSCLI_PPROF is set
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/antong314/whatscli-rs/backend/config"
	"github.com/antong314/whatscli-rs/backend/messages"
	"github.com/antong314/whatscli-rs/backend/server"
	"github.com/antong314/whatscli-rs/backend/transcribe"
	"github.com/antong314/whatscli-rs/backend/translate"

	pb "github.com/antong314/whatscli-rs/backend/gen/pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

func socketDir() string {
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		return filepath.Join(xdg, "whatscli")
	}
	return filepath.Join(os.TempDir(), "whatscli")
}

func socketPath() string {
	return filepath.Join(socketDir(), "whatscli.sock")
}

func pidPath() string {
	return filepath.Join(socketDir(), "whatscli.pid")
}

func writePID() error {
	dir := socketDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	return os.WriteFile(pidPath(), []byte(strconv.Itoa(os.Getpid())), 0600)
}

func cleanup() {
	os.Remove(socketPath())
	os.Remove(pidPath())
}

func main() {
	config.InitConfig()
	translate.SuppressStderr()
	// Must happen before any timers matter — see appnap_darwin.go.
	disableAppNap()

	// Always serve Go pprof on localhost for diagnosing hangs and stalls —
	// the supervisor discards our output, so this is the only live window
	// into the process: curl 'http://127.0.0.1:6161/debug/pprof/goroutine?debug=2'
	// (Fails silently if the port is taken, e.g. a second backend.)
	go func() { _ = http.ListenAndServe("127.0.0.1:6161", nil) }()

	broadcast := server.NewBroadcaster()
	handler := server.NewGrpcHandler(broadcast)

	sm := &messages.SessionManager{}
	sm.Init(handler)

	if config.Config.General.EnableTranslation {
		dialect := config.Config.General.TranslationDialect
		sm.Translator = translate.New(dialect)
		fmt.Println("Initializing translation model...")
		go func() {
			if err := sm.Translator.Init(config.Config.General.TranslationModelPath, nil); err != nil {
				fmt.Fprintf(os.Stderr, "translation init failed: %v\n", err)
			} else {
				fmt.Println("Translation model loaded")
			}
		}()
	}

	sm.Transcriber = transcribe.New()
	fmt.Println("Initializing transcription model...")
	go func() {
		if err := sm.Transcriber.Init("", nil); err != nil {
			fmt.Fprintf(os.Stderr, "transcription init failed: %v\n", err)
		} else {
			fmt.Println("Transcription model loaded")
		}
	}()

	sock := socketPath()
	if err := os.MkdirAll(socketDir(), 0700); err != nil {
		fmt.Fprintf(os.Stderr, "failed to create socket dir: %v\n", err)
		os.Exit(1)
	}
	os.Remove(sock)

	lis, err := net.Listen("unix", sock)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to listen on %s: %v\n", sock, err)
		os.Exit(1)
	}
	defer lis.Close()

	if err := writePID(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to write PID file: %v\n", err)
	}

	grpcServer := grpc.NewServer()
	svc := server.NewWhatsCLIServer(sm, broadcast, handler)
	pb.RegisterWhatsCLIServer(grpcServer, svc)
	reflection.Register(grpcServer)

	if err := sm.StartManager(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to start session manager: %v\n", err)
		os.Exit(1)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\nShutting down...")
		grpcServer.GracefulStop()
		cleanup()
	}()

	fmt.Printf("whatscli-server listening on %s\n", sock)
	if err := grpcServer.Serve(lis); err != nil {
		fmt.Fprintf(os.Stderr, "gRPC server error: %v\n", err)
		os.Exit(1)
	}
	cleanup()
}
