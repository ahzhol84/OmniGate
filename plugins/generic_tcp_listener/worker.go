package simple_http_responder

import (
	"context"
	"encoding/json"
	"fmt"

	"iot-middleware/pkg/auth"
	"iot-middleware/pkg/base"
	"iot-middleware/pkg/common"
	"iot-middleware/pkg/plugin"
	"log"
	"net/http"
	"sync"
	"time"
)

type ConfigItem struct {
	ListenAddr string `json:"listen_addr"`
	Username   string `json:"username"`
	Password   string `json:"password"`
	AuthKey    string `json:"auth_key"`
}

type SimpleHTTPResponder struct {
	configs []ConfigItem
	servers []*http.Server
}

func (w *SimpleHTTPResponder) Init(configs []json.RawMessage) error {
	for i, raw := range configs {
		var cfg ConfigItem
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return fmt.Errorf("config[%d]: %v", i, err)
		}
		if cfg.ListenAddr == "" {
			cfg.ListenAddr = ":8080"
		}
		w.configs = append(w.configs, cfg)
	}
	return nil
}

func (w *SimpleHTTPResponder) Start(ctx context.Context, out chan<- *base.DeviceData) {
	var wg sync.WaitGroup

	for i, cfg := range w.configs {
		wg.Add(1)
		go func(idx int, c ConfigItem) {
			defer wg.Done()
			w.run(ctx, c, idx)
		}(i, cfg)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-ctx.Done():
		for _, s := range w.servers {
			if s != nil {
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				s.Shutdown(shutdownCtx)
				cancel()
			}
		}
		<-done
	case <-done:
	}
}

func (w *SimpleHTTPResponder) run(ctx context.Context, cfg ConfigItem, index int) {
	validator, err := auth.NewValidator(auth.AuthConfig{
		Username: cfg.Username,
		Password: cfg.Password,
		AuthKey:  cfg.AuthKey,
	}, common.RDB, fmt.Sprintf("responder-%d", index+1))

	if err != nil {
		log.Fatalf("[RESPONDER-%d] auth init failed: %v", index+1, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/login", validator.LoginHandler())
	mux.HandleFunc("/hello", validator.AuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("hello world"))
	}))

	srv := &http.Server{Addr: cfg.ListenAddr, Handler: mux}
	w.servers = append(w.servers, srv)

	go func() {
		log.Printf("[RESPONDER-%d] listening on %s", index+1, cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[RESPONDER-%d] error: %v", index+1, err)
		}
	}()

	<-ctx.Done()
	srv.Shutdown(context.Background())
}

func init() {
	plugin.Register("simple_http_responder", func() base.IWorker {
		return &SimpleHTTPResponder{}
	})
}
