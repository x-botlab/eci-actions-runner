package cli

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/x-botlab/eci-actions-runner/internal/config"
	"github.com/x-botlab/eci-actions-runner/internal/github"
	"github.com/x-botlab/eci-actions-runner/internal/pool"
	"github.com/x-botlab/eci-actions-runner/internal/scheduler"
)

// runnerNamePrefixAll matches every runner this tool creates, whatever the pool.
const runnerNamePrefixAll = "eci-"

func newGitHubCmd(configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "github",
		Short: "Check the GitHub App wiring without touching ECI",
	}
	cmd.AddCommand(newGitHubCheckCmd(configPath), newGitHubJITCmd(configPath))
	return cmd
}

// boundRepos returns the repositories that receive runners, once each.
func boundRepos(cfg *config.Config) []github.Repo {
	seen := make(map[string]struct{})
	var repos []github.Repo
	for _, binding := range cfg.Bindings() {
		key := binding.Repo.String()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		repos = append(repos, binding.Repo)
	}
	return repos
}

func newForge(cfg *config.Config) (*github.AppClient, error) {
	return buildForge(cfg)
}

// hintFor turns an API failure into the one action that fixes it.
func hintFor(err error) string {
	message := err.Error()
	switch {
	case strings.Contains(message, "403"):
		return "permission denied: the App needs Actions:read and Administration:write"
	case strings.Contains(message, "404"):
		return "not found: the App is not installed on this repo, or the repo was not selected"
	case strings.Contains(message, "401"):
		return "bad credentials: check the App ID and the private key"
	}
	return ""
}

func newGitHubCheckCmd(configPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "check",
		Short: "Verify the App can read the queue and manage runners",
		Long: "Mints an installation token per repo, reports the permissions GitHub granted, and\n" +
			"exercises the read paths the scheduler depends on. Read-only: nothing is created\n" +
			"and nothing is deleted.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(*configPath)
			if err != nil {
				return err
			}
			forge, err := newForge(cfg)
			if err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			w := newTable(cmd)
			fmt.Fprintln(w, "REPO\tINSTALLATION\tTOKEN EXPIRES\tACTIONS\tADMINISTRATION\tQUEUED\tRUNNERS\tSTATUS")

			var failures []string
			for _, repo := range boundRepos(cfg) {
				row := []string{repo.String(), fmt.Sprint(repo.InstallationID), "-", "-", "-", "-", "-", "ok"}

				token, err := forge.Token(ctx, repo.InstallationID)
				if err != nil {
					row[7] = "token failed"
					failures = append(failures, fmt.Sprintf("%s: %v %s", repo, err, hintFor(err)))
					fmt.Fprintln(w, strings.Join(row, "\t"))
					continue
				}

				row[2] = token.Expires.Local().Format("15:04")
				row[3] = orNone(token.Permissions["actions"])
				row[4] = orNone(token.Permissions["administration"])

				var problems []string
				// Administration write is what minting a JIT config needs; read
				// alone fails only later, inside the launch path.
				if got := token.Permissions["administration"]; got != "write" {
					problems = append(problems, fmt.Sprintf("administration is %q, need write", got))
				}
				if got := token.Permissions["actions"]; got != "read" && got != "write" {
					problems = append(problems, fmt.Sprintf("actions is %q, need read", got))
				}

				jobs, err := forge.QueuedJobs(ctx, repo)
				if err != nil {
					problems = append(problems, fmt.Sprintf("queue read failed: %v %s", err, hintFor(err)))
				} else {
					row[5] = fmt.Sprint(len(jobs))
				}

				runners, err := forge.Runners(ctx, repo, runnerNamePrefixAll)
				if err != nil {
					problems = append(problems, fmt.Sprintf("runner list failed: %v %s", err, hintFor(err)))
				} else {
					row[6] = fmt.Sprint(len(runners))
				}

				if len(problems) > 0 {
					row[7] = "FAILED"
					failures = append(failures, fmt.Sprintf("%s: %s", repo, strings.Join(problems, "; ")))
				}
				fmt.Fprintln(w, strings.Join(row, "\t"))
			}

			if err := w.Flush(); err != nil {
				return err
			}
			if len(failures) > 0 {
				return fmt.Errorf("%d repo(s) failed:\n  %s", len(failures), strings.Join(failures, "\n  "))
			}
			return nil
		},
	}
}

func newGitHubJITCmd(configPath *string) *cobra.Command {
	var keep bool

	cmd := &cobra.Command{
		Use:   "jit",
		Short: "Create and immediately delete a JIT runner registration",
		Long: "Exercises the Administration:write path that launching depends on: mints a JIT\n" +
			"config per pool, confirms the registration exists, then removes it. The encoded\n" +
			"config is never printed.\n\n" +
			"A pool with jobs already queued is skipped, because a probe runner would claim\n" +
			"real work and then disappear with it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(*configPath)
			if err != nil {
				return err
			}
			forge, err := newForge(cfg)
			if err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			w := newTable(cmd)
			fmt.Fprintln(w, "REPO\tPOOL\tRUNNER\tRUNNER ID\tJIT\tSTATUS")

			var failures []string
			for _, binding := range cfg.Bindings() {
				row := []string{binding.Repo.String(), binding.Pool.Name, "-", "-", "-", ""}

				queued, err := forge.QueuedJobs(ctx, binding.Repo)
				if err != nil {
					row[5] = "queue read failed"
					failures = append(failures, fmt.Sprintf("%s: %v", binding.Repo, err))
					fmt.Fprintln(w, strings.Join(row, "\t"))
					continue
				}
				if waiting := countMatching(binding.Pool, queued); waiting > 0 {
					row[5] = fmt.Sprintf("skipped: %d job(s) waiting", waiting)
					fmt.Fprintln(w, strings.Join(row, "\t"))
					continue
				}

				name := fmt.Sprintf("%sjitcheck-%s", scheduler.RunnerNamePrefix(binding.Pool.Name), randomSuffix())
				jit, err := forge.CreateJITConfig(ctx, binding.Repo, name, binding.Pool.Labels)
				if err != nil {
					row[5] = "jit failed"
					failures = append(failures, fmt.Sprintf("%s/%s: %v %s",
						binding.Repo, binding.Pool.Name, err, hintFor(err)))
					fmt.Fprintln(w, strings.Join(row, "\t"))
					continue
				}

				row[2] = jit.RunnerName
				row[3] = fmt.Sprint(jit.RunnerID)
				row[4] = describeSecret(jit.Encoded)

				if keep {
					row[5] = "kept (delete it by hand)"
					fmt.Fprintln(w, strings.Join(row, "\t"))
					continue
				}

				if err := forge.RemoveRunner(ctx, binding.Repo, jit.RunnerID); err != nil {
					row[5] = fmt.Sprintf("delete failed, runner %d left behind", jit.RunnerID)
					failures = append(failures, fmt.Sprintf("%s/%s: %v",
						binding.Repo, binding.Pool.Name, err))
				} else {
					row[5] = "created and deleted"
				}
				fmt.Fprintln(w, strings.Join(row, "\t"))
			}

			if err := w.Flush(); err != nil {
				return err
			}
			if len(failures) > 0 {
				return fmt.Errorf("%d binding(s) failed:\n  %s", len(failures), strings.Join(failures, "\n  "))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&keep, "keep", false, "leave the registration in place for inspection")
	return cmd
}

// describeSecret proves a credential is present without revealing it.
func describeSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return fmt.Sprintf("%d bytes, sha256:%s", len(secret), hex.EncodeToString(sum[:4]))
}

func randomSuffix() string {
	buf := make([]byte, 3)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%06x", time.Now().UnixNano()&0xffffff)
	}
	return hex.EncodeToString(buf)
}

func orNone(value string) string {
	if value == "" {
		return "(none)"
	}
	return value
}

// countMatching mirrors the scheduler's queue accounting.
func countMatching(spec pool.Spec, queued []github.Job) int {
	waiting := 0
	for _, job := range queued {
		if spec.Matches(job.Labels) {
			waiting++
		}
	}
	return waiting
}
