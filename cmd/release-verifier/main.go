package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/shared/releaseintegrity"
)

const usageText = `release-verifier — offline release integrity verifier

Usage:
  release-verifier verify --media <read-only-dir> --trust-dir <host-trust-dir> [options]
  release-verifier check-installed --target <installed-dir> --trust-dir <host-trust-dir> [options]

Options:
  --applications <csv>   optional application codes to include in the plan
  --expected-release-digest <sha256:...>  optional installed release binding
  --output json|human    output format (default json)
  --require-provenance   require in-toto/SLSA provenance (default true)

The verifier never writes the media, starts containers, imports images, or
contacts a network. The trust directory must be provisioned independently of
the media being verified.`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usageText)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "verify":
		err = runVerify(os.Args[2:])
	case "check-installed":
		err = runCheckInstalled(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Println(usageText)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n%s\n", os.Args[1], usageText)
		os.Exit(2)
	}
	if err != nil {
		os.Exit(1)
	}
}

func runVerify(args []string) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	media := flags.String("media", "", "read-only release media directory")
	trustDir := flags.String("trust-dir", "", "independent host trust directory")
	applications := flags.String("applications", "", "optional comma-separated application codes")
	output := flags.String("output", "json", "json or human")
	requireProvenance := flags.Bool("require-provenance", true, "require in-toto/SLSA provenance")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *media == "" || *trustDir == "" {
		return usageError("--media and --trust-dir are required")
	}
	plan, err := releaseintegrity.VerifyMedia(releaseintegrity.VerifyOptions{
		MediaDir:          *media,
		TrustDir:          *trustDir,
		Applications:      splitCSV(*applications),
		RequireProvenance: *requireProvenance,
	})
	if err != nil {
		return emitFailure(err, *output)
	}
	return emitSuccess(plan.Status, plan, *output)
}

func runCheckInstalled(args []string) error {
	flags := flag.NewFlagSet("check-installed", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	target := flags.String("target", "", "installed deployment directory")
	trustDir := flags.String("trust-dir", "", "independent host trust directory")
	applications := flags.String("applications", "", "optional comma-separated application codes")
	output := flags.String("output", "json", "json or human")
	expectedDigest := flags.String("expected-release-digest", "", "optional sha256:<64hex> expected release digest")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *target == "" || *trustDir == "" {
		return usageError("--target and --trust-dir are required")
	}
	plan, err := releaseintegrity.CheckInstalledWithExpected(*target, *trustDir, splitCSV(*applications), strings.TrimSpace(*expectedDigest))
	if err != nil {
		return emitFailure(err, *output)
	}
	return emitSuccess(plan.Status, plan, *output)
}

func splitCSV(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func emitSuccess(status string, plan *releaseintegrity.VerifiedReleasePlan, output string) error {
	if output == "human" {
		fmt.Printf("status=%s release=%s digest=%s sequence=%d platform=%s\n", status, plan.ReleaseID, plan.ReleaseDigest, plan.Sequence, plan.Platform)
		for _, app := range plan.Applications {
			fmt.Printf("application=%s/%s components=%d\n", app.Code, app.Environment, len(app.Components))
		}
		return nil
	}
	payload := struct {
		Status string                                `json:"status"`
		Plan   *releaseintegrity.VerifiedReleasePlan `json:"plan"`
	}{Status: status, Plan: plan}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(payload)
}

func emitFailure(err error, output string) error {
	var policyError *releaseintegrity.Error
	code := releaseintegrity.CodeReleaseInvalid
	message := err.Error()
	details := map[string]any(nil)
	if errors.As(err, &policyError) {
		code = policyError.Code
		message = policyError.Message
		details = policyError.Details
	}
	if output == "human" {
		fmt.Fprintf(os.Stderr, "status=FAILED code=%s message=%s\n", code, message)
		return fmt.Errorf("release verification failed: %w", err)
	}
	payload := struct {
		Status  string         `json:"status"`
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details,omitempty"`
	}{Status: "FAILED", Code: string(code), Message: message, Details: details}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(payload)
	return fmt.Errorf("release verification failed: %w", err)
}

func usageError(message string) error {
	fmt.Fprintf(os.Stderr, "%s\n\n%s\n", message, usageText)
	return errors.New(message)
}
