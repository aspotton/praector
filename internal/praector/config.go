package praector

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

var ProfileNames = []string{"coordinator", "explore", "critic", "plan_review", "research", "implement", "review", "advise"}
var ReadOnlyProfiles = map[string]bool{"coordinator": true, "explore": true, "critic": true, "plan_review": true, "research": true, "review": true, "advise": true}
var WriteProfiles = map[string]bool{"implement": true}

var ProfileTitles = map[string]string{
	"coordinator": "Coordinator / Planner",
	"explore":     "Repository Explorer",
	"critic":      "Pre-planning Critic",
	"plan_review": "Plan Reviewer",
	"research":    "Researcher",
	"implement":   "Implementation Worker",
	"review":      "Implementation Reviewer",
	"advise":      "Strategic Advisor",
}

type Limits struct {
	MaxTaskDepth          int `json:"max_task_depth"`
	MaxTasksPerCollection int `json:"max_tasks_per_collection"`
	MaxChildrenPerTask    int `json:"max_children_per_task"`
	MaxParallelTasks      int `json:"max_parallel_tasks"`
	MaxAttemptsPerTask    int `json:"max_attempts_per_task"`
	TaskTimeoutSeconds    int `json:"task_timeout_seconds"`
}

func DefaultLimits() Limits { return Limits{8, 100, 10, 4, 3, 3600} }

type ProjectConfig struct {
	ProjectID       string
	Name            string
	Root            string
	BaseBranch      string
	Harness         string
	OpenCodeCommand string
	Models          map[string]string
	Limits          Limits
}

func (p ProjectConfig) ModelFor(profile string) string    { return strings.TrimSpace(p.Models[profile]) }
func (p ProjectConfig) ResolvedOpenCode() (string, error) { return RequireBinary(p.OpenCodeCommand) }

type GlobalConfig struct{ Home string }

func (g GlobalConfig) StateDir() string      { return filepath.Join(g.Home, "state") }
func (g GlobalConfig) LockFile() string      { return filepath.Join(g.Home, "state.lock") }
func (g GlobalConfig) SchedulerLock() string { return filepath.Join(g.Home, "scheduler.lock") }
func (g GlobalConfig) WorktreesDir() string  { return filepath.Join(g.Home, "worktrees") }
func (g GlobalConfig) LogsDir() string       { return filepath.Join(g.Home, "logs") }

func Global() (GlobalConfig, error) {
	home := os.Getenv("PRAECTOR_HOME")
	if home == "" {
		home = expandHome("~/.praector")
	}
	home = expandHome(home)
	abs, err := filepath.Abs(home)
	if err != nil {
		return GlobalConfig{}, err
	}
	for _, p := range []string{abs, filepath.Join(abs, "worktrees"), filepath.Join(abs, "logs")} {
		if err := os.MkdirAll(p, 0755); err != nil {
			return GlobalConfig{}, err
		}
	}
	return GlobalConfig{Home: abs}, nil
}

func FindProjectMarker(start string) (string, bool) {
	if start == "" {
		start, _ = os.Getwd()
	}
	abs, err := filepath.Abs(start)
	if err != nil {
		return "", false
	}
	if st, e := os.Stat(abs); e == nil && !st.IsDir() {
		abs = filepath.Dir(abs)
	}
	for {
		c := filepath.Join(abs, ".praector", "project.toml")
		if st, e := os.Stat(c); e == nil && !st.IsDir() {
			return c, true
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			break
		}
		abs = parent
	}
	return "", false
}

func trimTOMLString(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		if u, err := strconv.Unquote(v); err == nil {
			return u
		}
		return strings.Trim(v, "\"")
	}
	return v
}

// parseMarkerTOML parses only the stable, deliberately small project.toml
// contract written by Praector. Avoiding a general TOML dependency keeps the core
// binary statically buildable and dependency-light.
func parseMarkerTOML(path string) (map[string]map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]map[string]string{"": {}}
	section := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			if out[section] == nil {
				out[section] = map[string]string{}
			}
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[section][strings.TrimSpace(k)] = trimTOMLString(v)
	}
	return out, sc.Err()
}

func intOr(s string, d int) int {
	if i, e := strconv.Atoi(strings.TrimSpace(s)); e == nil {
		return i
	}
	return d
}

func ReadProjectMarker(path string) (ProjectConfig, error) {
	d, err := parseMarkerTOML(path)
	if err != nil {
		return ProjectConfig{}, err
	}
	pr := d["project"]
	ws := d["workspace"]
	h := d["harness"]
	md := d["models"]
	lr := d["limits"]
	if pr["id"] == "" || pr["name"] == "" {
		return ProjectConfig{}, fmt.Errorf("invalid project marker: missing project id/name")
	}
	rootv := ws["repository"]
	if rootv == "" {
		rootv = "../.."
	}
	if !filepath.IsAbs(rootv) {
		rootv = filepath.Join(filepath.Dir(path), rootv)
	}
	root, _ := filepath.Abs(rootv)
	lim := DefaultLimits()
	lim.MaxTaskDepth = intOr(lr["max_task_depth"], lim.MaxTaskDepth)
	lim.MaxTasksPerCollection = intOr(lr["max_tasks_per_collection"], lim.MaxTasksPerCollection)
	lim.MaxChildrenPerTask = intOr(lr["max_children_per_task"], lim.MaxChildrenPerTask)
	lim.MaxParallelTasks = intOr(lr["max_parallel_tasks"], lim.MaxParallelTasks)
	lim.MaxAttemptsPerTask = intOr(lr["max_attempts_per_task"], lim.MaxAttemptsPerTask)
	lim.TaskTimeoutSeconds = intOr(lr["task_timeout_seconds"], lim.TaskTimeoutSeconds)
	models := map[string]string{}
	for _, n := range ProfileNames {
		models[n] = md[n]
	}
	harness := h["default"]
	if harness == "" {
		harness = "opencode"
	}
	cmd := h["command"]
	if cmd == "" {
		cmd = "opencode"
	}
	base := ws["base_branch"]
	if base == "" {
		base = "main"
	}
	return ProjectConfig{ProjectID: pr["id"], Name: pr["name"], Root: filepath.Clean(root), BaseBranch: base, Harness: harness, OpenCodeCommand: cmd, Models: models, Limits: lim}, nil
}

func tomlString(v string) string { return strconv.Quote(v) }
func WriteProjectMarker(c ProjectConfig) (string, error) {
	dir := filepath.Join(c.Root, ".praector")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	p := filepath.Join(dir, "project.toml")
	var b strings.Builder
	fmt.Fprintln(&b, "version = 1\n\n[project]")
	fmt.Fprintf(&b, "id = %s\nname = %s\n\n", tomlString(c.ProjectID), tomlString(c.Name))
	fmt.Fprintln(&b, "[workspace]")
	fmt.Fprintln(&b, "type = \"git\"")
	fmt.Fprintln(&b, "repository = \"..\"")
	fmt.Fprintf(&b, "base_branch = %s\n\n", tomlString(c.BaseBranch))
	fmt.Fprintln(&b, "[harness]\ndefault = \"opencode\"")
	fmt.Fprintf(&b, "command = %s\n\n[models]\n", tomlString(c.OpenCodeCommand))
	for _, n := range ProfileNames {
		fmt.Fprintf(&b, "%s = %s\n", n, tomlString(c.Models[n]))
	}
	l := c.Limits
	fmt.Fprintf(&b, "\n[limits]\nmax_task_depth = %d\nmax_tasks_per_collection = %d\nmax_children_per_task = %d\nmax_parallel_tasks = %d\nmax_attempts_per_task = %d\ntask_timeout_seconds = %d\n", l.MaxTaskDepth, l.MaxTasksPerCollection, l.MaxChildrenPerTask, l.MaxParallelTasks, l.MaxAttemptsPerTask, l.TaskTimeoutSeconds)
	return p, os.WriteFile(p, []byte(b.String()), 0644)
}

func ProjectConfigJSON(c ProjectConfig) Row {
	return Row{"harness": c.Harness, "opencode_command": c.OpenCodeCommand, "models": c.Models, "limits": Row{"max_task_depth": c.Limits.MaxTaskDepth, "max_tasks_per_collection": c.Limits.MaxTasksPerCollection, "max_children_per_task": c.Limits.MaxChildrenPerTask, "max_parallel_tasks": c.Limits.MaxParallelTasks, "max_attempts_per_task": c.Limits.MaxAttemptsPerTask, "task_timeout_seconds": c.Limits.TaskTimeoutSeconds}}
}

func ProjectConfigFromRow(r Row) ProjectConfig {
	raw := M(r, "config")
	if raw == nil {
		raw = JSONMap(S(r, "config_json"))
	}
	lm := M(raw, "limits")
	lim := DefaultLimits()
	if lm != nil {
		if I(lm, "max_task_depth") > 0 {
			lim.MaxTaskDepth = I(lm, "max_task_depth")
		}
		if I(lm, "max_tasks_per_collection") > 0 {
			lim.MaxTasksPerCollection = I(lm, "max_tasks_per_collection")
		}
		if I(lm, "max_children_per_task") > 0 {
			lim.MaxChildrenPerTask = I(lm, "max_children_per_task")
		}
		if I(lm, "max_parallel_tasks") > 0 {
			lim.MaxParallelTasks = I(lm, "max_parallel_tasks")
		}
		if I(lm, "max_attempts_per_task") > 0 {
			lim.MaxAttemptsPerTask = I(lm, "max_attempts_per_task")
		}
		if I(lm, "task_timeout_seconds") > 0 {
			lim.TaskTimeoutSeconds = I(lm, "task_timeout_seconds")
		}
	}
	models := map[string]string{}
	mm := M(raw, "models")
	for _, n := range ProfileNames {
		if mm != nil {
			models[n] = S(mm, n)
		}
	}
	h := S(raw, "harness")
	if h == "" {
		h = "opencode"
	}
	cmd := S(raw, "opencode_command")
	if cmd == "" {
		cmd = "opencode"
	}
	root, _ := filepath.Abs(S(r, "root_path"))
	return ProjectConfig{ProjectID: S(r, "id"), Name: S(r, "name"), Root: root, BaseBranch: S(r, "base_branch"), Harness: h, OpenCodeCommand: cmd, Models: models, Limits: lim}
}

func MarshalPretty(v any) string { b, _ := json.MarshalIndent(v, "", "  "); return string(b) }
