package praector

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode"
)

func UTCNow() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000000") }

func MakeID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)[:10]
}

func NormalizeText(v string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(v)), " "))
}

func DedupeKey(profile, objective string) string {
	h := sha256.Sum256([]byte(profile + "\n" + NormalizeText(objective)))
	return hex.EncodeToString(h[:])[:32]
}

func JSONDumps(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func JSONMap(s string) Row {
	if strings.TrimSpace(s) == "" {
		return Row{}
	}
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var m Row
	if dec.Decode(&m) != nil {
		return Row{}
	}
	return m
}

func SQLLiteral(v any) string {
	if v == nil {
		return "NULL"
	}
	switch x := v.(type) {
	case bool:
		if x {
			return "1"
		}
		return "0"
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case json.Number:
		return x.String()
	default:
		s := fmt.Sprint(v)
		s = strings.ReplaceAll(s, `\`, `\\`)
		s = strings.ReplaceAll(s, `'`, `''`)
		return `'` + s + `'`
	}
}

func RequireBinary(name string) (string, error) {
	if strings.ContainsAny(name, `/\\`) {
		p, err := filepath.Abs(expandHome(name))
		if err != nil {
			return "", err
		}
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			return "", fmt.Errorf("required executable not found or not executable: %s", p)
		}
		return p, nil
	}
	p, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("required executable not found on PATH: %s", name)
	}
	return p, nil
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		h, _ := os.UserHomeDir()
		if p == "~" {
			return h
		}
		return filepath.Join(h, p[2:])
	}
	return p
}

type CmdResult struct {
	Code           int
	Stdout, Stderr string
}

func Run(ctx context.Context, args []string, cwd string, env map[string]string, input string, check bool) (CmdResult, error) {
	if len(args) == 0 {
		return CmdResult{}, errors.New("empty command")
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	if cwd != "" {
		cmd.Dir = cwd
	}
	if env != nil {
		merged := os.Environ()
		idx := map[string]int{}
		sep := "="
		for i, e := range merged {
			if k, _, ok := strings.Cut(e, sep); ok {
				idx[strings.ToUpper(k)] = i
			}
		}
		for k, v := range env {
			kv := k + "=" + v
			if i, ok := idx[strings.ToUpper(k)]; ok {
				merged[i] = kv
			} else {
				merged = append(merged, kv)
			}
		}
		cmd.Env = merged
	}
	if input != "" {
		cmd.Stdin = strings.NewReader(input)
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else if errors.Is(err, context.DeadlineExceeded) {
			code = 124
		} else {
			return CmdResult{Code: -1, Stdout: out.String(), Stderr: errb.String()}, err
		}
	}
	r := CmdResult{Code: code, Stdout: out.String(), Stderr: errb.String()}
	if check && code != 0 {
		detail := strings.TrimSpace(r.Stderr)
		if detail == "" {
			detail = strings.TrimSpace(r.Stdout)
		}
		return r, fmt.Errorf("command failed (%d): %s\n%s", code, strings.Join(args, " "), detail)
	}
	return r, nil
}

func DeepMerge(base, over Row) Row {
	out := CopyRow(base)
	for k, v := range over {
		bm, bok := out[k].(map[string]any)
		om, ook := v.(map[string]any)
		if bok && ook {
			out[k] = DeepMerge(Row(bm), Row(om))
			continue
		}
		out[k] = v
	}
	return out
}

func BoolEnv(name string, def bool) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(name)))
	if v == "" {
		return def
	}
	return !(v == "0" || v == "false" || v == "no" || v == "off")
}

func PathListWithout(pathList string, roots ...string) string {
	out := []string{}
	for _, p := range filepath.SplitList(pathList) {
		keep := true
		for _, r := range roots {
			if r == "" {
				continue
			}
			rp, _ := filepath.Abs(r)
			pp, _ := filepath.Abs(p)
			rel, err := filepath.Rel(rp, pp)
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
				keep = false
				break
			}
		}
		if keep && p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, string(os.PathListSeparator))
}

func IsWindows() bool { return runtime.GOOS == "windows" }

func HumanTitle(s string, max int) string {
	fields := strings.FieldsFunc(strings.TrimSpace(s), func(r rune) bool { return unicode.IsSpace(r) })
	out := strings.Join(fields, " ")
	if len(out) > max {
		out = out[:max]
	}
	return out
}
