package taskengine

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestGoalFixtureOpenRejectsFallbackBeforeDial(t *testing.T) {
	t.Setenv("BBA_FIXTURE_ONLY", "1")
	for _, dsn := range []string{
		"host=127.0.0.1,203.0.113.19 port=1,5432 user=bba_dev dbname=bba_dev connect_timeout=1",
		"host=203.0.113.19 user=bba_dev dbname=bba_dev connect_timeout=1",
		"host=localhost user=bba_dev dbname=bba_dev connect_timeout=1",
	} {
		s, err := OpenGoalFixture(context.Background(), dsn)
		if s != nil {
			s.Close()
			t.Fatal("unsafe pool opened")
		}
		if err == nil || !strings.Contains(err.Error(), "local database endpoints only") {
			t.Fatalf("unsafe endpoint was not rejected before dial: %v", err)
		}
	}
}

func TestGoalFixturePoolRevalidatesEveryConnection(t *testing.T) {
	g, _, _ := goalFixture(t)
	cfg := g.store.Pool.Config()
	cfg.MaxConns = 1
	cfg.MinConns = 0
	if err := configureGoalFixturePool(cfg); err != nil {
		t.Fatal(err)
	}
	// A later endpoint change must still be refused by the connection hook.
	changed := cfg.ConnConfig.Copy()
	changed.Host = "203.0.113.19"
	if err := cfg.BeforeConnect(context.Background(), changed); err == nil {
		t.Fatal("new connection bypassed endpoint check")
	}
	original := cfg.AfterConnect
	var calls atomic.Int32
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		calls.Add(1)
		return original(ctx, conn)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for i := 0; i < 2; i++ {
		var head int
		if err := pool.QueryRow(context.Background(), "SELECT max(version) FROM core.schema_version").Scan(&head); err != nil || head != SchemaHead() {
			t.Fatalf("fixture connection rejected: head=%d err=%v", head, err)
		}
		pool.Reset()
	}
	if calls.Load() != 2 {
		t.Fatalf("expected actual peer/schema/identity checks for both connections, got %d", calls.Load())
	}
}

func TestGoalFixtureRejectsUnexpectedActualPeer(t *testing.T) {
	for _, peer := range []net.Addr{&net.TCPAddr{IP: net.ParseIP("203.0.113.19"), Port: 5432}, &net.UnixAddr{Name: "/tmp/.s.PGSQL.5432", Net: "unix"}, nil} {
		if goalFixturePeer(peer) {
			t.Fatal("nonfixture actual peer accepted")
		}
	}
	for _, peer := range []net.Addr{&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 5432}, &net.TCPAddr{IP: net.ParseIP("::1"), Port: 5432}, &net.UnixAddr{Name: "/fixture/.s.PGSQL.5432", Net: "unix"}} {
		if !goalFixturePeer(peer) {
			t.Fatal("local fixture peer rejected")
		}
	}
}
