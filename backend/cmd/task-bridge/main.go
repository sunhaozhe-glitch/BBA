package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"bba.local/taskengine/internal/taskengine"
)

const maxGatewayConfigBytes = 1 << 20

type gatewayRuntimeConfig struct {
	TokenSHA256 string `json:"token_sha256"`
	WorkspaceID string `json:"workspace_id"`
	RuntimeID   string `json:"runtime_id"`
	AccountID   string `json:"account_id"`
	OwnerEpoch  int64  `json:"owner_epoch"`
	OneBotURL   string `json:"onebot_url"`
	OneBotToken string `json:"onebot_token"`
	WebUIURL    string `json:"webui_url"`
	WebUIToken  string `json:"webui_token"`
}

type gatewayConfig struct {
	Runtimes []gatewayRuntimeConfig `json:"runtimes"`
}

type gatewayAuth struct {
	byDigest map[string]taskengine.RuntimeActor
}

type gatewayServer struct {
	runtimes map[string]napcatGateway
}

func main() {
	listenAddr := flag.String("listen", "127.0.0.1:18422", "loopback listen address")
	origin := flag.String("origin", "http://127.0.0.1:18422", "fixed Task Bridge origin")
	configPath := flag.String("config", "", "mode-0600 Task Bridge runtime and NapCat credential file")
	flag.Parse()
	if *configPath == "" {
		log.Fatal("-config is required")
	}
	if _, err := validateGatewayOrigin(*origin, *listenAddr); err != nil {
		log.Fatal(err)
	}
	config, err := loadGatewayConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	server, auth, err := newGateway(config)
	if err != nil {
		log.Fatal(err)
	}
	dsn := os.Getenv("BBA_TASKENGINE_DSN")
	if dsn == "" {
		log.Fatal("BBA_TASKENGINE_DSN is required")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	store, err := taskengine.Open(ctx, dsn)
	if err != nil {
		log.Fatalf("open Task Engine store: %v", err)
	}
	defer store.Close()
	if err := validateGatewaySchema(store); err != nil {
		log.Fatal(err)
	}
	listener, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	if tcp, ok := listener.Addr().(*net.TCPAddr); !ok || !tcp.IP.IsLoopback() {
		log.Fatal("Task Bridge listener must bind to loopback")
	}
	bridgeHandler := store.BridgeRuntimeHandler(auth.authenticate, server)
	chatHandler := store.ChatSendGatewayHandler(auth.authenticate, server)
	mux := http.NewServeMux()
	mux.Handle("/v1/internal/bridge/", bridgeHandler)
	mux.Handle("/v1/internal/chat-send-permits", chatHandler)
	mux.Handle("/v1/internal/chat-send-permits/", chatHandler)
	handler := bridgeFixedHost(mux, *origin)
	httpServer := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	go func() {
		<-ctx.Done()
		shutdown, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		_ = httpServer.Shutdown(shutdown)
	}()
	fmt.Printf("task-bridge listening origin=%s runtimes=%d schema=22 pid=%d\n", *origin, len(config.Runtimes), os.Getpid())
	if err := httpServer.Serve(listener); err != nil && err != http.ErrServerClosed {
		log.Fatalf("serve Task Bridge: %v", err)
	}
}

func loadGatewayConfig(path string) (gatewayConfig, error) {
	var config gatewayConfig
	info, err := os.Lstat(path)
	if err != nil {
		return config, fmt.Errorf("stat Task Bridge config: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > maxGatewayConfigBytes {
		return config, errors.New("Task Bridge config must be regular, bounded, and mode 0600 or stricter")
	}
	file, err := os.Open(path)
	if err != nil {
		return config, fmt.Errorf("open Task Bridge config: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, maxGatewayConfigBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return config, errors.New("invalid Task Bridge config")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF || len(config.Runtimes) == 0 || len(config.Runtimes) > 64 {
		return config, errors.New("Task Bridge config must contain 1..64 runtime entries")
	}
	return config, nil
}

func newGateway(config gatewayConfig) (*gatewayServer, *gatewayAuth, error) {
	server := &gatewayServer{runtimes: make(map[string]napcatGateway, len(config.Runtimes))}
	auth := &gatewayAuth{byDigest: make(map[string]taskengine.RuntimeActor, len(config.Runtimes))}
	for _, entry := range config.Runtimes {
		if !validGatewayID(entry.WorkspaceID) || !validGatewayID(entry.RuntimeID) || !validGatewayID(entry.AccountID) || entry.OwnerEpoch <= 0 ||
			!validSHA256(entry.TokenSHA256) || strings.TrimSpace(entry.OneBotToken) == "" || strings.TrimSpace(entry.WebUIToken) == "" {
			return nil, nil, errors.New("Task Bridge runtime entry is incomplete")
		}
		oneBotURL, err := validateGatewayLoopbackURL(entry.OneBotURL)
		if err != nil {
			return nil, nil, errors.New("Task Bridge OneBot endpoint must be a fixed loopback URL")
		}
		webUIURL, err := validateGatewayLoopbackURL(entry.WebUIURL)
		if err != nil {
			return nil, nil, errors.New("Task Bridge WebUI endpoint must be a fixed loopback URL")
		}
		key := gatewayRuntimeKey(entry.WorkspaceID, entry.RuntimeID)
		if _, exists := server.runtimes[key]; exists {
			return nil, nil, errors.New("Task Bridge runtime mapping is duplicated")
		}
		if _, exists := auth.byDigest[entry.TokenSHA256]; exists {
			return nil, nil, errors.New("Task Bridge runtime token digest is duplicated")
		}
		server.runtimes[key] = napcatGateway{config: entry, oneBotURL: oneBotURL, webUIURL: webUIURL, client: newGatewayHTTPClient()}
		auth.byDigest[entry.TokenSHA256] = taskengine.RuntimeActor{WorkspaceID: entry.WorkspaceID,
			RuntimeID: entry.RuntimeID, OwnerEpoch: entry.OwnerEpoch, AccountIDs: []string{entry.AccountID}}
	}
	return server, auth, nil
}

func (a *gatewayAuth) authenticate(request *http.Request) (taskengine.RuntimeActor, error) {
	if len(request.Header.Values("Authorization")) != 1 {
		return taskengine.RuntimeActor{}, &taskengine.Error{Code: "UNAUTHENTICATED"}
	}
	header := request.Header.Get("Authorization")
	if len(header) <= len("Bearer ") || !strings.EqualFold(header[:len("Bearer ")], "Bearer ") {
		return taskengine.RuntimeActor{}, &taskengine.Error{Code: "UNAUTHENTICATED"}
	}
	token := header[len("Bearer "):]
	if strings.TrimSpace(token) != token || strings.ContainsAny(token, " \t\r\n") {
		return taskengine.RuntimeActor{}, &taskengine.Error{Code: "UNAUTHENTICATED"}
	}
	digest := sha256.Sum256([]byte(token))
	encoded := hex.EncodeToString(digest[:])
	for expected, actor := range a.byDigest {
		if subtle.ConstantTimeCompare([]byte(encoded), []byte(expected)) == 1 {
			return actor, nil
		}
	}
	return taskengine.RuntimeActor{}, &taskengine.Error{Code: "UNAUTHENTICATED"}
}

func (g *gatewayServer) runtime(actor taskengine.RuntimeActor) (napcatGateway, error) {
	runtime, ok := g.runtimes[gatewayRuntimeKey(actor.WorkspaceID, actor.RuntimeID)]
	if !ok || runtime.config.OwnerEpoch != actor.OwnerEpoch || len(actor.AccountIDs) != 1 || actor.AccountIDs[0] != runtime.config.AccountID {
		return napcatGateway{}, &taskengine.Error{Code: "RESOURCE_NOT_FOUND"}
	}
	return runtime, nil
}

func (g *gatewayServer) ObserveIdentity(ctx context.Context, actor taskengine.RuntimeActor, withGroups bool) (taskengine.BridgeIdentity, error) {
	runtime, err := g.runtime(actor)
	if err != nil {
		return taskengine.BridgeIdentity{}, err
	}
	return runtime.observeIdentity(ctx, withGroups)
}

func (g *gatewayServer) CleanerAvailable(ctx context.Context, actor taskengine.RuntimeActor) error {
	runtime, err := g.runtime(actor)
	if err != nil {
		return err
	}
	return runtime.cleanerAvailable(ctx)
}

func (g *gatewayServer) ObserveMembership(ctx context.Context, actor taskengine.RuntimeActor, groupID string) (bool, error) {
	runtime, err := g.runtime(actor)
	if err != nil {
		return false, err
	}
	return runtime.observeMembership(ctx, groupID)
}

func (g *gatewayServer) CheckIdentity(ctx context.Context, actor taskengine.RuntimeActor, reservation taskengine.Reservation) error {
	runtime, err := g.runtime(actor)
	if err != nil {
		return err
	}
	identity, err := runtime.observeIdentity(ctx, true)
	if err != nil {
		return err
	}
	if identity.SelfID == "" || !identity.Online || !identity.Good || containsString(identity.GroupIDs, reservation.GroupID) {
		return errors.New("runtime identity or target membership precondition failed")
	}
	return runtime.cleanerAvailable(ctx)
}

func (g *gatewayServer) RequestJoin(ctx context.Context, actor taskengine.RuntimeActor, reservation taskengine.Reservation) (taskengine.SendOutcome, error) {
	runtime, err := g.runtime(actor)
	if err != nil {
		return taskengine.SendOutcome{Disposition: "unknown", EffectCertainty: "unknown"}, err
	}
	return runtime.requestJoin(ctx, reservation)
}

func (g *gatewayServer) SendChatText(ctx context.Context, actor taskengine.RuntimeActor, kind, conversationID, text string) (string, error) {
	runtime, err := g.runtime(actor)
	if err != nil {
		return "", err
	}
	return runtime.SendChatText(ctx, actor, kind, conversationID, text)
}

func validateGatewayOrigin(origin, listenAddr string) (string, error) {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.Host != listenAddr || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("origin must be fixed http loopback URL matching listen address")
	}
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil || port == "" || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return "", errors.New("Task Bridge must bind to a loopback IP and port")
	}
	return parsed.Host, nil
}

func validateGatewaySchema(store *taskengine.Store) error {
	var version int
	if err := store.Pool.QueryRow(context.Background(), `SELECT max(version) FROM core.schema_version`).Scan(&version); err != nil {
		return fmt.Errorf("read Task Engine schema version: %w", err)
	}
	// B3 final payload review requires the content tables, not merely the
	// legacy bridge tables. Reject old and unknown future heads, never migrate
	// implicitly on gateway startup.
	if version != taskengine.SchemaHead() {
		return fmt.Errorf("Task Engine requires schema %d; found %d", taskengine.SchemaHead(), version)
	}
	return nil
}

func bridgeFixedHost(next http.Handler, origin string) http.Handler {
	expected, _ := url.Parse(origin)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != expected.Host || r.URL.Host != "" || r.URL.Scheme != "" {
			http.Error(w, "misdirected request", http.StatusMisdirectedRequest)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func gatewayRuntimeKey(workspace, runtime string) string { return workspace + "\x00" + runtime }

func validGatewayID(value string) bool {
	if value == "" || len(value) > 128 || strings.TrimSpace(value) != value {
		return false
	}
	for _, char := range value {
		if char < 0x21 || char > 0x7e {
			return false
		}
	}
	return true
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
