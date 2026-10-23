package infrastructure

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestProductionDatabaseBackupShell(t *testing.T) {
	t.Parallel()
	// 仅使用文件系统夹具和模拟转储执行备份脚本；
	// 此测试不执行 Docker 命令，也不连接数据库。
	for _, scenario := range []string{"complete", "dump-failed", "empty", "checksum-create-failed", "checksum-failed", "publish-failed", "collision", "symlink"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			bin := filepath.Join(root, "bin")
			backups := filepath.Join(root, "backups")
			for _, path := range []string{bin, backups} {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			dump := `#!/bin/sh
test "$MYSQL_PWD" = "fixture-only-secret" || exit 83
case " $* " in *" --all-databases "*) ;; *) exit 84;; esac
case " $* " in *" --events "*) ;; *) exit 85;; esac
case "$CASE" in
 dump-failed) printf 'partial'; exit 86;;
 empty) exit 0;;
 *) printf '%s\n' 'CREATE DATABASE contract_management;' 'CREATE DATABASE temporal;' 'CREATE DATABASE temporal_visibility;' 'CREATE DATABASE dashboard_metabase;';;
esac
`
			if err := os.WriteFile(filepath.Join(bin, "mysqldump"), []byte(dump), 0o700); err != nil {
				t.Fatal(err)
			}
			checksum, err := exec.LookPath("sha256sum")
			if err != nil {
				t.Fatal("sha256sum is required for the production backup regression")
			}
			sha := fmt.Sprintf("#!/bin/sh\nif test \"$CASE\" = checksum-create-failed; then exit 87; fi\nif test \"$CASE\" = checksum-failed && test \"$1\" = -c; then exit 87; fi\nexec %q \"$@\"\n", checksum)
			if err := os.WriteFile(filepath.Join(bin, "sha256sum"), []byte(sha), 0o700); err != nil {
				t.Fatal(err)
			}
			if scenario == "publish-failed" {
				if err := os.WriteFile(filepath.Join(bin, "mv"), []byte("#!/bin/sh\nexit 88\n"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			name := "contract_management-onboarding-fixture.sql"
			final := filepath.Join(backups, name+".backup")
			if scenario == "collision" {
				if err := os.Mkdir(final, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(final, "preserved"), []byte("previous"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "symlink" {
				link := filepath.Join(root, "linked-backups")
				if err := os.Symlink(backups, link); err != nil {
					t.Fatal(err)
				}
				backups = link
			}
			command := exec.Command("sh", "-c", productionDatabaseBackupCommand, "_", name, backups)
			command.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "MYSQL_ROOT_PASSWORD=fixture-only-secret", "CASE="+scenario)
			output, err := command.CombinedOutput()
			if scenario == "complete" {
				if err != nil {
					t.Fatalf("backup failed: %v: %s", err, output)
				}
				content, err := os.ReadFile(filepath.Join(final, name))
				if err != nil {
					t.Fatal(err)
				}
				manifest, err := os.ReadFile(filepath.Join(final, "SHA256SUMS"))
				if err != nil {
					t.Fatal(err)
				}
				want := fmt.Sprintf("%x  %s\n", sha256.Sum256(content), name)
				if string(manifest) != want {
					t.Fatalf("manifest = %q, want %q", manifest, want)
				}
				for path, mode := range map[string]os.FileMode{final: 0o700, filepath.Join(final, name): 0o600, filepath.Join(final, "SHA256SUMS"): 0o600} {
					info, err := os.Stat(path)
					if err != nil {
						t.Fatal(err)
					}
					if info.Mode().Perm() != mode {
						t.Fatalf("unsafe mode %o for %s", info.Mode().Perm(), path)
					}
				}
			} else {
				if err == nil {
					t.Fatal("failed backup unexpectedly succeeded")
				}
				if scenario != "collision" {
					if _, err := os.Stat(final); !os.IsNotExist(err) {
						t.Fatalf("failed backup published final directory: %v", err)
					}
				}
			}
			partials, err := filepath.Glob(filepath.Join(backups, "."+name+".*"))
			if err != nil {
				t.Fatal(err)
			}
			if len(partials) != 0 {
				t.Fatalf("partial backups were not cleaned: %v", partials)
			}
			if strings.Contains(string(output), "fixture-only-secret") {
				t.Fatal("credential appeared in output")
			}
			if scenario == "collision" {
				content, err := os.ReadFile(filepath.Join(final, "preserved"))
				if err != nil || string(content) != "previous" {
					t.Fatal("previous recovery point changed")
				}
			}
		})
	}
}

type productionBackupFailureRunner struct{ recordingSubsystemRunner }

func (runner *productionBackupFailureRunner) Run(ctx context.Context, directory string, environment []string, binary string, arguments ...string) error {
	if err := runner.recordingSubsystemRunner.Run(ctx, directory, environment, binary, arguments...); err != nil {
		return err
	}
	if strings.Contains(strings.Join(arguments, " "), "mysqldump") {
		return errors.New("backup failed with a fixture secret that must not surface")
	}
	return nil
}

func TestProductionDatabaseBackupFailureBlocksMigration(t *testing.T) {
	t.Parallel()
	runner := &productionBackupFailureRunner{}
	provisioner, _ := productionProvisionerFixtureWithRunner(t, false, runner)
	err := provisioner.Provision(context.Background(), productionContractInput("https://platform.example.com"))
	if err == nil || !strings.Contains(err.Error(), "backup production subsystem database") {
		t.Fatalf("expected backup failure, got %v", err)
	}
	if strings.Contains(err.Error(), "fixture secret") {
		t.Fatal("backup error leaked command details")
	}
	backupSeen := false
	for _, call := range runner.calls {
		if strings.Contains(strings.Join(call.arguments, " "), "mysqldump") {
			backupSeen = true
		}
		for _, denied := range []string{"contract-migrate", "contract-api", "contract-catalog-sync"} {
			if containsString(call.arguments, denied) {
				t.Fatalf("backup failure allowed later step: %v", call.arguments)
			}
		}
	}
	if !backupSeen {
		t.Fatal("failure test did not execute backup")
	}
}

func TestProductionDatabaseBackupRejectsHostSymlink(t *testing.T) {
	t.Parallel()
	provisioner, runner, _ := productionProvisionerFixture(t)
	target, err := provisioner.target(testProductionApplicationCode, testProductionEnvironment)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(target.config.DeployRoot, "backups")); err != nil {
		t.Fatal(err)
	}
	err = provisioner.Provision(context.Background(), productionContractInput("https://platform.example.com"))
	if err == nil || !strings.Contains(err.Error(), "backup directory is unsafe") {
		t.Fatalf("expected unsafe backup directory error, got %v", err)
	}
	for _, call := range runner.calls {
		if strings.Contains(strings.Join(call.arguments, " "), "mysqldump") || containsString(call.arguments, "contract-migrate") || containsString(call.arguments, "contract-api") {
			t.Fatalf("unsafe directory allowed backup or later deployment: %v", call.arguments)
		}
	}
}
