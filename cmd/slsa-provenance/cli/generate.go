package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/peterbourgon/ff/v3/ffcli"
	"github.com/pkg/errors"

	"github.com/philips-labs/slsa-provenance-action/lib/github"
	"github.com/philips-labs/slsa-provenance-action/lib/intoto"
)

const (
	gitHubHostedIDSuffix = "/Attestations/GitHubHostedActions@v1"
	selfHostedIDSuffix   = "/Attestations/SelfHostedActions@v1"
	typeID               = "https://github.com/Attestations/GitHubActionsWorkflow@v1"
	payloadContentType   = "application/vnd.in-toto+json"
)

// RequiredFlagError creates an error flag error for the given flag name
func RequiredFlagError(flagName string) error {
	return fmt.Errorf("no value found for required flag: %s", flagName)
}

// subjects walks the file or directory at "root" and hashes all files.
func subjects(root string) ([]intoto.Subject, error) {
	var s []intoto.Subject
	return s, filepath.Walk(root, func(abspath string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		relpath, err := filepath.Rel(root, abspath)
		if err != nil {
			return err
		}
		// Note: filepath.Rel() returns "." when "root" and "abspath" point to the same file.
		if relpath == "." {
			relpath = filepath.Base(root)
		}
		contents, err := os.ReadFile(abspath)
		if err != nil {
			return err
		}
		sha := sha256.Sum256(contents)
		shaHex := hex.EncodeToString(sha[:])
		s = append(s, intoto.Subject{Name: relpath, Digest: intoto.DigestSet{"sha256": shaHex}})
		return nil
	})
}

func builderID(repoURI string) string {
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		return repoURI + gitHubHostedIDSuffix
	}
	return repoURI + selfHostedIDSuffix
}

// Generate creates an instance of *ffcli.Command to generate provenance
func Generate(w io.Writer) *ffcli.Command {
	var (
		flagset       = flag.NewFlagSet("slsa-provenance generate", flag.ExitOnError)
		artifactPath  = flagset.String("artifact_path", "", "The file or dir path of the artifacts for which provenance should be generated.")
		outputPath    = flagset.String("output_path", "build.provenance", "The path to which the generated provenance should be written.")
		githubContext = flagset.String("github_context", "", "The '${github}' context value.")
		runnerContext = flagset.String("runner_context", "", "The '${runner}' context value.")
	)

	flagset.SetOutput(w)

	return &ffcli.Command{
		Name:       "generate",
		ShortUsage: "slsa-provenance generate",
		ShortHelp:  "Generates the slsa provenance file",
		FlagSet:    flagset,
		Exec: func(ctx context.Context, args []string) error {
			if *artifactPath == "" {
				flagset.Usage()
				return RequiredFlagError("-artifact_path")
			}
			if *outputPath == "" {
				flagset.Usage()
				return RequiredFlagError("-output_path")
			}
			if *githubContext == "" {
				flagset.Usage()
				return RequiredFlagError("-github_context")
			}
			if *runnerContext == "" {
				flagset.Usage()
				return RequiredFlagError("-runner_context")
			}

			subjects, err := subjects(*artifactPath)
			if os.IsNotExist(err) {
				return fmt.Errorf("resource path not found: [provided=%s]", *artifactPath)
			} else if err != nil {
				return err
			}

			anyCtx := github.AnyContext{}
			if err := json.Unmarshal([]byte(*githubContext), &anyCtx.Context); err != nil {
				return errors.Wrap(err, "failed to unmarshal github context json")
			}
			if err := json.Unmarshal([]byte(*runnerContext), &anyCtx.RunnerContext); err != nil {
				return errors.Wrap(err, "failed to unmarshal runner context json")
			}
			gh := anyCtx.Context

			// NOTE: Re-runs are not uniquely identified and can cause run ID collisions.
			repoURI := "https://github.com/" + gh.Repository

			stmt := intoto.SLSAProvenanceStatement(
				intoto.WithSubject(subjects),
				intoto.WithBuilder(builderID(repoURI)),
				intoto.WithMetadata(repoURI+"/actions/runs/"+gh.RunID),
			)

			stmt.Predicate.Recipe = intoto.Recipe{
				Type:              typeID,
				DefinedInMaterial: 0,
			}
			stmt.Predicate.Materials = []intoto.Item{}

			// NOTE: This is inexact as multiple workflows in a repo can have the same name.
			// See https://github.com/github/feedback/discussions/4188
			stmt.Predicate.Recipe.EntryPoint = gh.Workflow
			event := github.AnyEvent{}
			if err := json.Unmarshal(gh.Event, &event); err != nil {
				return errors.Wrap(err, "failed to unmarshal github context event json")
			}

			stmt.Predicate.Recipe.Arguments = event.Inputs
			stmt.Predicate.Materials = append(stmt.Predicate.Materials, intoto.Item{URI: "git+" + repoURI, Digest: intoto.DigestSet{"sha1": gh.SHA}})

			// NOTE: At L1, writing the in-toto Statement type is sufficient but, at
			// higher SLSA levels, the Statement must be encoded and wrapped in an
			// Envelope to support attaching signatures.
			payload, _ := json.MarshalIndent(stmt, "", "  ")
			fmt.Fprintf(w, "Saving provenance to %s:\n\n%s\n", *outputPath, string(payload))

			if err := os.WriteFile(*outputPath, payload, 0755); err != nil {
				return errors.Wrap(err, "failed to write provenance")
			}

			return nil
		},
	}
}
