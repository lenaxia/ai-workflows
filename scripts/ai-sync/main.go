// Command sync is the renderer + propagator for lenaxia/ai-workflows.
//
// It reads consumer config files (consumers/<name>.yaml), renders templates
// from templates/prompts/ using Go's text/template (supports {{ .Var }} and
// {{ block "name" . }}...{{ end }} for per-consumer overrides), and writes
// the rendered files into a target directory.
//
// Usage:
//
//	ai-sync render --consumer gokore --into /path/to/gokore
//	ai-sync render --all --into /tmp/rendered
//	ai-sync diff --consumer gokore --into /path/to/gokore   # show what would change
//
// The renderer is intentionally dependency-free (stdlib only): no gomplate,
// no external YAML parser. Consumer configs use a minimal YAML subset parsed
// by parseConsumerConfig.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	var (
		consumer string
		into     string
		all      bool
		repoRoot string
	)
	switch cmd {
	case "render":
		fs.StringVar(&consumer, "consumer", "", "consumer name (without .yaml)")
		fs.StringVar(&into, "into", "", "target directory to write rendered files")
		fs.BoolVar(&all, "all", false, "render all consumers")
		fs.StringVar(&repoRoot, "repo-root", ".", "ai-workflows repo root (auto-detected from binary location if empty)")
		fs.Parse(os.Args[2:])
		if !all && consumer == "" {
			fail("--consumer or --all is required")
		}
		if into == "" {
			fail("--into is required")
		}
		root := resolveRoot(repoRoot)
		consumers := []string{consumer}
		if all {
			consumers = listConsumers(root)
		}
		for _, c := range consumers {
			if err := renderConsumer(root, c, into); err != nil {
				fail("render %s: %v", c, err)
			}
		}
	case "diff":
		fs.StringVar(&consumer, "consumer", "", "consumer name")
		fs.StringVar(&into, "into", "", "target directory to compare against")
		fs.StringVar(&repoRoot, "repo-root", ".", "ai-workflows repo root")
		fs.Parse(os.Args[2:])
		if consumer == "" {
			fail("--consumer is required")
		}
		if into == "" {
			fail("--into is required")
		}
		root := resolveRoot(repoRoot)
		if err := diffConsumer(root, consumer, into); err != nil {
			fail("diff %s: %v", consumer, err)
		}
	case "list":
		fs.StringVar(&repoRoot, "repo-root", ".", "ai-workflows repo root")
		fs.Parse(os.Args[2:])
		root := resolveRoot(repoRoot)
		for _, c := range listConsumers(root) {
			fmt.Println(c)
		}
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `ai-sync — render and propagate lenaxia/ai-workflows

Commands:
  render --consumer <name> --into <dir>   render one consumer's files into <dir>
  render --all --into <dir>               render all consumers into <dir>/<name>/
  diff   --consumer <name> --into <dir>   show files that would change
  list                                   list configured consumers

Consumer configs live at <root>/consumers/<name>.yaml.
Templates live at <root>/templates/prompts/*.md.
Per-consumer override blocks live at <root>/consumers/<name>/*.md.`)
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

// renderConsumer renders every template for the given consumer into into/.
// Files classified as "forked" in the consumer config are skipped with a
// printed warning. Files classified as "consumer-owned" are never templated
// in the first place (they don't live in templates/).
func renderConsumer(root, consumer, into string) error {
	cfg, err := loadConsumerConfig(root, consumer)
	if err != nil {
		return err
	}
	tmplDir := filepath.Join(root, "templates", "prompts")
	entries, err := os.ReadDir(tmplDir)
	if err != nil {
		return fmt.Errorf("read templates: %w", err)
	}
	rendered := 0
	skipped := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".md")
		if cfg.isForked(name) {
			fmt.Fprintf(os.Stderr, "WARN: %s.md marked forked by %s — skipping (consumer owns this file)\n", name, consumer)
			skipped++
			continue
		}
		out, err := renderTemplate(root, tmplDir, e.Name(), cfg)
		if err != nil {
			return fmt.Errorf("template %s: %w", e.Name(), err)
		}
		outPath := filepath.Join(into, e.Name())
		if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(outPath, out, 0o644); err != nil {
			return err
		}
		rendered++
	}
	fmt.Printf("rendered %d files for %s into %s (%d forked-skipped)\n", rendered, consumer, into, skipped)
	return nil
}

// renderTemplate parses one template file, registers any per-consumer
// override blocks from consumers/<name>/, and executes it.
func renderTemplate(root, tmplDir, filename string, cfg consumerConfig) ([]byte, error) {
	tmplPath := filepath.Join(tmplDir, filename)
	raw, err := os.ReadFile(tmplPath)
	if err != nil {
		return nil, err
	}

	// Collect override files: consumers/<name>/<blockname>.md for every
	// {{ block "blockname" . }} referenced in the template.
	overrideDir := filepath.Join(root, "consumers", cfg.Name)
	tmpl, err := template.New(filename).
		Option("missingkey=error").
		Funcs(template.FuncMap{}).
		Parse(string(raw))
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}

	// Parse override files that define blocks present in the template.
	for _, block := range cfg.Blocks() {
		overridePath := filepath.Join(overrideDir, block+".md")
		if _, err := os.Stat(overridePath); err == nil {
			if _, err := tmpl.ParseFiles(overridePath); err != nil {
				return nil, fmt.Errorf("override %s: %w", block, err)
			}
		}
	}

	var buf strings.Builder
	if err := tmpl.Execute(&buf, cfg.Vars()); err != nil {
		return nil, fmt.Errorf("execute: %w", err)
	}
	out := buf.String()
	// Prepend managed banner unless the template already carries one.
	banner := managedBanner(cfg.Version)
	if !strings.Contains(out, banner) {
		out = banner + "\n" + out
	}
	return []byte(out), nil
}

// managedBanner is the discoverability header prepended to every rendered
// file. Answers "can I edit this?" for engineers browsing consumer repos.
func managedBanner(version string) string {
	return fmt.Sprintf("<!-- Managed by lenaxia/ai-workflows@%s — do not edit. Override via consumers/<repo>.yaml. -->", version)
}

// ---------------------------------------------------------------------------
// Diff
// ---------------------------------------------------------------------------

// diffConsumer renders into a temp dir and reports which files differ from
// the target, without writing anything. Used by the propagate workflow to
// decide whether a PR is needed.
func diffConsumer(root, consumer, into string) error {
	tmp, err := os.MkdirTemp("", "ai-sync-diff-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	if err := renderConsumer(root, consumer, tmp); err != nil {
		return err
	}
	fmt.Printf("\n--- diff for %s against %s ---\n", consumer, into)
	return walkDiff(tmp, into)
}

func walkDiff(renderedDir, targetDir string) error {
	return filepath.Walk(renderedDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, err := filepath.Rel(renderedDir, path)
		if err != nil {
			return err
		}
		targetPath := filepath.Join(targetDir, rel)
		rendered, _ := os.ReadFile(path)
		existing, readErr := os.ReadFile(targetPath)
		switch {
		case readErr != nil && os.IsNotExist(readErr):
			fmt.Printf("+ %s (new, %d bytes)\n", rel, len(rendered))
		case readErr != nil:
			return readErr
		case string(rendered) != string(existing):
			fmt.Printf("~ %s (changed)\n", rel)
		default:
			fmt.Printf("  %s (unchanged)\n", rel)
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Consumer config (minimal YAML subset, stdlib-only)
// ---------------------------------------------------------------------------

// consumerConfig is the per-repo contract. It maps consumer name → variables
// for templates, lists forked files (consumer owns, sync skips), and lists
// block overrides (per-consumer fragments that fill template {{ block }}s).
type consumerConfig struct {
	Name      string
	Version   string
	vars      map[string]string
	forked    map[string]bool
	blocks    []string // override block names referenced by templates
}

func (c consumerConfig) Vars() map[string]string { return c.vars }
func (c consumerConfig) isForked(file string) bool {
	return c.forked[file] || c.forked[file+".md"]
}
func (c consumerConfig) Blocks() []string { return c.blocks }

// loadConsumerConfig reads consumers/<name>.yaml. The format is intentionally
// minimal:
//
//	name: gokore
//	version: v1.0.0
//	vars:
//	  project_name: gokore
//	  rules_doc: README-LLM.md
//	forked:
//	  - help.md
//	blocks:
//	  - project_rules
//	  - testing_notes
func loadConsumerConfig(root, name string) (consumerConfig, error) {
	path := filepath.Join(root, "consumers", name+".yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		return consumerConfig{}, fmt.Errorf("read %s: %w", path, err)
	}
	cfg := consumerConfig{
		Name:    name,
		vars:    map[string]string{},
		forked:  map[string]bool{},
		Version: "main", // default; overridden by var or pin
	}
	section := ""
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		// Detect top-level keys.
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			if k, v, ok := splitKV(trimmed); ok {
				switch k {
				case "name":
					cfg.Name = v
				case "version":
					cfg.Version = v
				case "vars", "forked", "blocks":
					section = k
					continue
				default:
					cfg.vars[k] = v
				}
				section = ""
				continue
			}
		}
		// Section items.
		switch section {
		case "vars":
			if k, v, ok := splitKV(trimmed); ok {
				cfg.vars[k] = v
				if k == "version" {
					cfg.Version = v
				}
			}
		case "forked":
			item := strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))
			if item != "" {
				cfg.forked[item] = true
			}
		case "blocks":
			item := strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))
			if item != "" {
				cfg.blocks = append(cfg.blocks, item)
			}
		}
	}
	// Default project_name to consumer name if not set.
	if cfg.vars["project_name"] == "" {
		cfg.vars["project_name"] = cfg.Name
	}
	return cfg, nil
}

func splitKV(s string) (k, v string, ok bool) {
	idx := strings.Index(s, ":")
	if idx < 0 {
		return "", "", false
	}
	k = strings.TrimSpace(s[:idx])
	v = strings.TrimSpace(s[idx+1:])
	v = strings.Trim(v, `"'`)
	return k, v, true
}

// ---------------------------------------------------------------------------
// Repo helpers
// ---------------------------------------------------------------------------

func resolveRoot(explicit string) string {
	if explicit != "" && explicit != "." {
		abs, err := filepath.Abs(explicit)
		if err == nil {
			return abs
		}
	}
	// Auto-detect: binary lives at <root>/scripts/ai-sync (or is run from root).
	if cwd, err := os.Getwd(); err == nil {
		if _, err := os.Stat(filepath.Join(cwd, "templates", "prompts")); err == nil {
			return cwd
		}
		if _, err := os.Stat(filepath.Join(cwd, "consumers")); err == nil {
			return cwd
		}
	}
	return explicit
}

func listConsumers(root string) []string {
	entries, err := os.ReadDir(filepath.Join(root, "consumers"))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml") {
			out = append(out, strings.TrimSuffix(strings.TrimSuffix(name, ".yaml"), ".yml"))
		}
	}
	return out
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "ai-sync: "+format+"\n", args...)
	os.Exit(1)
}
