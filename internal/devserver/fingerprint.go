package devserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

type launchDefinition struct {
	Project             string            `json:"project"`
	Target              string            `json:"target"`
	Command             string            `json:"command"`
	BindHost            string            `json:"bind_host"`
	ProbeHost           string            `json:"probe_host"`
	PublicHost          string            `json:"public_host"`
	PreferredPort       int               `json:"preferred_port"`
	PortRange           *PortRange        `json:"port_range,omitempty"`
	HealthPath          string            `json:"health_path"`
	ReadyText           string            `json:"ready_text"`
	IdentityHeader      string            `json:"identity_header"`
	IdentityHeaderValue string            `json:"identity_header_value"`
	ReusePolicy         ReusePolicy       `json:"reuse_policy"`
	StartupTimeout      string            `json:"startup_timeout"`
	HealthInterval      string            `json:"health_interval"`
	Environment         map[string]string `json:"environment,omitempty"`
	FingerprintPaths    []string          `json:"fingerprint_paths,omitempty"`
}

func ResolveLaunchFingerprint(ctx context.Context, manifest *ServeManifest) (LaunchFingerprint, error) {
	definitionHash, err := hashLaunchDefinition(manifest)
	if err != nil {
		return LaunchFingerprint{}, err
	}
	repoHead, repoDirty, gitHash, err := hashGitCheckout(ctx, manifest.RepoRoot)
	if err != nil {
		return LaunchFingerprint{}, err
	}
	explicitHash, err := hashExplicitPaths(manifest.RepoRoot, manifest.FingerprintPaths)
	if err != nil {
		return LaunchFingerprint{}, err
	}
	source := sha256.New()
	writeHashPart(source, "git", gitHash)
	writeHashPart(source, "explicit", explicitHash)
	sourceHash := hex.EncodeToString(source.Sum(nil))

	fingerprint := sha256.New()
	writeHashPart(fingerprint, "schema", FingerprintSchemaV1)
	writeHashPart(fingerprint, "definition", definitionHash)
	writeHashPart(fingerprint, "head", repoHead)
	writeHashPart(fingerprint, "source", sourceHash)
	return LaunchFingerprint{
		SchemaVersion:        FingerprintSchemaV1,
		Value:                hex.EncodeToString(fingerprint.Sum(nil)),
		RepoHead:             repoHead,
		RepoDirty:            repoDirty,
		LaunchDefinitionHash: definitionHash,
		SourceHash:           sourceHash,
	}, nil
}

func hashLaunchDefinition(manifest *ServeManifest) (string, error) {
	paths := append([]string(nil), manifest.FingerprintPaths...)
	slices.Sort(paths)
	data, err := json.Marshal(launchDefinition{
		Project:             manifest.Project,
		Target:              manifest.Target,
		Command:             manifest.Command,
		BindHost:            manifest.BindHost,
		ProbeHost:           manifest.ProbeHost,
		PublicHost:          manifest.PublicHost,
		PreferredPort:       manifest.PreferredPort,
		PortRange:           manifest.PortRange,
		HealthPath:          manifest.HealthPath,
		ReadyText:           manifest.ReadyText,
		IdentityHeader:      manifest.IdentityHeader,
		IdentityHeaderValue: manifest.IdentityHeaderValue,
		ReusePolicy:         manifest.ReusePolicy,
		StartupTimeout:      manifest.StartupTimeout.String(),
		HealthInterval:      manifest.HealthInterval.String(),
		Environment:         manifest.Environment,
		FingerprintPaths:    paths,
	})
	if err != nil {
		return "", fmt.Errorf("encoding resolved launch definition: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func hashGitCheckout(ctx context.Context, repoRoot string) (string, bool, string, error) {
	head, err := gitOutput(ctx, repoRoot, "rev-parse", "--verify", "HEAD")
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && unversionedCheckoutError(err) {
			return "", false, hashBytes(nil), nil
		}
		return "", false, "", err
	}
	diff, err := gitOutput(ctx, repoRoot, "diff", "--no-ext-diff", "--binary", "HEAD", "--")
	if err != nil {
		return "", false, "", err
	}
	untracked, err := gitOutput(ctx, repoRoot, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return "", false, "", err
	}
	hash := sha256.New()
	writeHashPart(hash, "diff", string(diff))
	for _, relative := range splitNullTerminated(untracked) {
		if err := hashPath(hash, repoRoot, relative); err != nil {
			return "", false, "", err
		}
	}
	return strings.TrimSpace(string(head)), len(diff) > 0 || len(untracked) > 0, hex.EncodeToString(hash.Sum(nil)), nil
}

func gitOutput(ctx context.Context, repoRoot string, args ...string) ([]byte, error) {
	commandArgs := append([]string{"-C", repoRoot}, args...)
	cmd := exec.CommandContext(ctx, "git", commandArgs...)
	output, err := cmd.Output()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, errors.New("git is required to fingerprint a versioned checkout")
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(string(exitErr.Stderr)), err)
		}
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return output, nil
}

func hashExplicitPaths(repoRoot string, paths []string) (string, error) {
	hash := sha256.New()
	sorted := append([]string(nil), paths...)
	slices.Sort(sorted)
	for _, relative := range sorted {
		if err := hashPath(hash, repoRoot, filepath.Clean(relative)); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func hashPath(hash io.Writer, repoRoot string, relative string) error {
	path := filepath.Join(repoRoot, relative)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		writeHashPart(hash, "missing", filepath.ToSlash(relative))
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading fingerprint path %s: %w", relative, err)
	}
	if !info.IsDir() {
		return hashFile(hash, path, relative, info)
	}
	return filepath.WalkDir(path, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		child, err := filepath.Rel(repoRoot, current)
		if err != nil {
			return err
		}
		childInfo, err := entry.Info()
		if err != nil {
			return err
		}
		return hashFile(hash, current, child, childInfo)
	})
}

func hashFile(hash io.Writer, path string, relative string, info os.FileInfo) error {
	writeHashPart(hash, "path", filepath.ToSlash(relative))
	writeHashPart(hash, "mode", info.Mode().String())
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return fmt.Errorf("reading fingerprint symlink %s: %w", relative, err)
		}
		writeHashPart(hash, "symlink", target)
		return nil
	}
	if !info.Mode().IsRegular() {
		writeHashPart(hash, "special", "")
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening fingerprint path %s: %w", relative, err)
	}
	defer func() { _ = file.Close() }()
	contentHash := sha256.New()
	if _, err := io.Copy(contentHash, file); err != nil {
		return fmt.Errorf("hashing fingerprint path %s: %w", relative, err)
	}
	writeHashPart(hash, "content", hex.EncodeToString(contentHash.Sum(nil)))
	return nil
}

func splitNullTerminated(data []byte) []string {
	raw := strings.Split(string(data), "\x00")
	result := make([]string, 0, len(raw))
	for _, value := range raw {
		if value != "" {
			result = append(result, value)
		}
	}
	slices.Sort(result)
	return result
}

func writeHashPart(writer io.Writer, label string, value string) {
	_, _ = io.WriteString(writer, label)
	_, _ = io.WriteString(writer, "\x00")
	_, _ = io.WriteString(writer, value)
	_, _ = io.WriteString(writer, "\x00")
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func unversionedCheckoutError(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "not a git repository") ||
		strings.Contains(message, "unknown revision or path not in the working tree") ||
		strings.Contains(message, "ambiguous argument 'head'") ||
		strings.Contains(message, "needed a single revision")
}

func fingerprintDriftReason(launch LaunchFingerprint, current LaunchFingerprint) string {
	switch {
	case launch.Value == "":
		return "running session predates launch fingerprinting"
	case launch.LaunchDefinitionHash != current.LaunchDefinitionHash:
		return "resolved launch definition changed"
	case launch.RepoHead != current.RepoHead:
		return "repo HEAD changed"
	case launch.SourceHash != current.SourceHash:
		return "dirty or explicit fingerprint inputs changed"
	default:
		return "launch fingerprint changed"
	}
}
