package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

// Run the production main in a child so log.Fatal, flag parsing and startup
// ordering are exercised without changing the parent test process.
func TestGoalFixtureProviderStartupChild(t *testing.T) {
	if os.Getenv("BBA_GOAL_PROVIDER_REVIEW_CHILD") != "1" {
		return
	}
	separator := slices.Index(os.Args, "--")
	if separator < 0 {
		t.Fatal("missing child argument separator")
	}
	os.Args = append([]string{"taskengine-server"}, os.Args[separator+1:]...)
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	log.SetFlags(0)
	main()
	os.Exit(0)
}

func goalProviderMainOutput(t *testing.T, args, environment []string) string {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, append([]string{"-test.run=^TestGoalFixtureProviderStartupChild$", "--"}, args...)...)
	// Never inherit a real DSN, libpq configuration, runtime credentials or
	// another startup test's child marker. Test DSNs below cannot connect.
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "BBA_") || strings.HasPrefix(name, "PG") || name == "DATABASE_URL" {
			continue
		}
		cmd.Env = append(cmd.Env, entry)
	}
	cmd.Env = append(cmd.Env, "BBA_GOAL_PROVIDER_REVIEW_CHILD=1")
	cmd.Env = append(cmd.Env, environment...)
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatal("main did not reject startup within the child timeout")
	}
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 1 {
		t.Fatalf("want main exit 1, got %v; output=%s", err, output)
	}
	return strings.TrimSpace(string(output))
}

func TestGoalFixtureRejectsLiveProviderFilesBeforeStartup(t *testing.T) {
	const privatePath = "/fixture/provider-review-path-secret"
	const privateDSN = "provider-review-invalid-dsn-secret"
	resolver := writeResolverFixture(t, 0600, `{"credentials":[{"token_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","workspace_id":"provider-review","principal_id":"operator-review","permissions":["operator.read"]}]}`)
	providers := []struct {
		name string
		args []string
	}{
		{"artex", []string{"-artex-provider-file=" + privatePath}},
		{"login", []string{"-login-provider-file=" + privatePath}},
		{"model", []string{"-model-provider-file=" + privatePath}},
		{"combined", []string{"-artex-provider-file=" + privatePath, "-login-provider-file=" + privatePath, "-model-provider-file=" + privatePath}},
	}
	startup := []struct {
		name string
		args []string
		env  []string
	}{
		{"missing_configuration", nil, nil},
		{"unreadable_resolver", []string{"-resolver-file=" + privatePath}, []string{"BBA_TASKENGINE_DSN=" + privateDSN}},
		{"invalid_database_configuration", []string{"-resolver-file=" + resolver}, []string{"BBA_TASKENGINE_DSN=" + privateDSN}},
	}
	for _, provider := range providers {
		for _, configuration := range startup {
			t.Run(provider.name+"/"+configuration.name, func(t *testing.T) {
				args := []string{"-offline-goal-fixture", "-development-operations", "-model-registry-file=" + privatePath}
				args = append(args, provider.args...)
				args = append(args, configuration.args...)
				output := goalProviderMainOutput(t, args, configuration.env)
				if output != "offline-goal-fixture forbids live provider files" {
					t.Fatalf("want secret-safe fixture rejection before startup inputs, got %q", output)
				}
				for _, secret := range []string{privatePath, privateDSN, resolver} {
					if strings.Contains(output, secret) {
						t.Fatal("fixture rejection exposed a startup path or credential")
					}
				}
			})
		}
	}
}

func TestGoalFixtureProviderGatePreservesOtherStartupConfigurations(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"ordinary_artex", []string{"-artex-provider-file=/fixture/unread-provider"}},
		{"ordinary_login", []string{"-login-provider-file=/fixture/unread-provider"}},
		{"ordinary_model", []string{"-model-provider-file=/fixture/unread-provider"}},
		{"explicit_nonfixture_combined", []string{"-offline-goal-fixture=false", "-artex-provider-file=/fixture/unread-provider", "-login-provider-file=/fixture/unread-provider", "-model-provider-file=/fixture/unread-provider"}},
		{"fixture_registry", []string{"-offline-goal-fixture", "-model-registry-file=/fixture/unread-registry"}},
		{"fixture_empty_provider_files", []string{"-offline-goal-fixture", "-artex-provider-file=", "-login-provider-file=", "-model-provider-file="}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			output := goalProviderMainOutput(t, tc.args, nil)
			if output != "BBA_TASKENGINE_DSN and -resolver-file are required" {
				t.Fatalf("want existing required-configuration check, got %q", output)
			}
		})
	}
}

func TestGoalFixtureStartupUsesIsolatedDatabaseOpen(t *testing.T) {
	resolver := writeResolverFixture(t, 0600, `{"credentials":[{"token_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","workspace_id":"provider-review","principal_id":"operator-review","permissions":["operator.read"]}]}`)
	const invalidDSN = "provider-review-invalid-dsn"
	for _, fixture := range []bool{false, true} {
		name := "ordinary"
		args := []string{"-resolver-file=" + resolver}
		if fixture {
			name = "fixture"
			args = append(args, "-offline-goal-fixture")
		}
		t.Run(name, func(t *testing.T) {
			output := goalProviderMainOutput(t, args, []string{"BBA_TASKENGINE_DSN=" + invalidDSN})
			if fixture {
				if output != "open Task Engine store: offline-goal-fixture requires explicit isolated development fixture" {
					t.Fatalf("want isolated database open to reject missing fixture environment before DSN parsing, got %q", output)
				}
			} else if !strings.Contains(output, "cannot parse `"+invalidDSN+"`") {
				t.Fatalf("ordinary main no longer uses its existing database open path: %q", output)
			}
		})
	}
}
