// Command fakeollama runs a stub Ollama API for local development when
// no real inference engine is installed. Dev/testing tool only — it
// echoes prompts back as completions.
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/localmesh/localmesh/internal/testutil/fakeollama"
)

func main() {
	port := flag.Int("port", 11435, "listen port")
	flag.Parse()

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "fakeollama listening on http://127.0.0.1:%d (dev stub — echoes prompts)\n", *port)
	fakeollama.ServeOn(ln,
		fakeollama.Model{Name: "llama3.1:8b", Family: "llama", Parameter: "8B", Size: 4 << 30},
		fakeollama.Model{Name: "qwen2.5:7b", Family: "qwen", Parameter: "7B", Size: 4 << 30},
		fakeollama.Model{Name: "nomic-embed-text", Family: "nomic-bert", Parameter: "137M", Size: 274 << 20},
	)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
}
