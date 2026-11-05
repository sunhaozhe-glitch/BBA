package taskengine

import (
	"context"
	"errors"
	"net"
	"path/filepath"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func goalFixtureHost(host string) bool {
	ip := net.ParseIP(host)
	return host == "/fixture" || (ip != nil && ip.IsLoopback())
}

func validateGoalFixtureConnConfig(cfg *pgx.ConnConfig) error {
	if !goalFixtureHost(cfg.Host) {
		return errors.New("offline-goal-fixture requires local database endpoints only")
	}
	for _, fallback := range cfg.Fallbacks {
		if fallback == nil || !goalFixtureHost(fallback.Host) {
			return errors.New("offline-goal-fixture requires local database endpoints only")
		}
	}
	if cfg.User != "bba_fixture" && (cfg.User != "bba_dev" || cfg.Database != "bba_dev") {
		return errors.New("offline-goal-fixture database identity is not an isolated fixture")
	}
	return nil
}

func goalFixturePeer(addr net.Addr) bool {
	switch peer := addr.(type) {
	case *net.TCPAddr:
		return peer.IP.IsLoopback()
	case *net.UnixAddr:
		return filepath.Dir(peer.Name) == "/fixture"
	default:
		return false
	}
}

func validateGoalFixtureConnection(ctx context.Context, conn *pgx.Conn) error {
	if !goalFixturePeer(conn.PgConn().Conn().RemoteAddr()) {
		return errors.New("offline-goal-fixture connected to a nonlocal database endpoint")
	}
	var head int
	var user, db string
	if err := conn.QueryRow(ctx, "SELECT (SELECT max(version) FROM core.schema_version),current_user,current_database()").Scan(&head, &user, &db); err != nil {
		return err
	}
	if head != SchemaHead() {
		return errors.New("offline-goal-fixture requires current schema; apply explicit migration first")
	}
	if user != "bba_fixture" && (user != "bba_dev" || db != "bba_dev") {
		return errors.New("offline-goal-fixture database identity is not an isolated fixture")
	}
	return nil
}

func configureGoalFixturePool(cfg *pgxpool.Config) error {
	if err := validateGoalFixtureConnConfig(cfg.ConnConfig); err != nil {
		return err
	}
	cfg.BeforeConnect = func(_ context.Context, conn *pgx.ConnConfig) error { return validateGoalFixtureConnConfig(conn) }
	cfg.AfterConnect = validateGoalFixtureConnection
	return nil
}
