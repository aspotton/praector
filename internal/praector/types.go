package praector

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

type Row map[string]any

type Rows []Row

func S(r Row, key string) string {
	if r == nil {
		return ""
	}
	v, ok := r[key]
	if !ok || v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		if x {
			return "1"
		}
		return "0"
	default:
		return fmt.Sprint(x)
	}
}

func I(r Row, key string) int {
	s := S(r, key)
	i, _ := strconv.Atoi(strings.TrimSpace(s))
	return i
}

func B(r Row, key string) bool {
	if r == nil {
		return false
	}
	switch x := r[key].(type) {
	case bool:
		return x
	case float64:
		return x != 0
	case json.Number:
		i, _ := x.Int64()
		return i != 0
	case string:
		x = strings.TrimSpace(strings.ToLower(x))
		return x == "1" || x == "true" || x == "yes" || x == "on"
	default:
		return false
	}
}

func M(r Row, key string) Row {
	if r == nil {
		return nil
	}
	switch x := r[key].(type) {
	case map[string]any:
		return Row(x)
	case Row:
		return x
	case string:
		var m Row
		if json.Unmarshal([]byte(x), &m) == nil {
			return m
		}
	}
	return nil
}

func Slice(r Row, key string) []any {
	if r == nil {
		return nil
	}
	switch x := r[key].(type) {
	case []any:
		return x
	case string:
		var a []any
		if json.Unmarshal([]byte(x), &a) == nil {
			return a
		}
	}
	return nil
}

func CopyRow(r Row) Row {
	out := Row{}
	for k, v := range r {
		out[k] = v
	}
	return out
}

const (
	TaskPending    = "PENDING"
	TaskReady      = "READY"
	TaskRunning    = "RUNNING"
	TaskWaiting    = "WAITING"
	TaskVerifying  = "VERIFYING"
	TaskComplete   = "COMPLETE"
	TaskFailed     = "FAILED"
	TaskCancelled  = "CANCELLED"
	TaskSuperseded = "SUPERSEDED"

	AttemptCreated   = "CREATED"
	AttemptStarting  = "STARTING"
	AttemptRunning   = "RUNNING"
	AttemptSucceeded = "SUCCEEDED"
	AttemptFailed    = "FAILED"
	AttemptTimedOut  = "TIMED_OUT"
	AttemptAborted   = "ABORTED"
)

var TerminalTaskStatuses = map[string]bool{TaskComplete: true, TaskFailed: true, TaskCancelled: true, TaskSuperseded: true}
var ActiveTaskStatuses = map[string]bool{TaskPending: true, TaskReady: true, TaskRunning: true, TaskWaiting: true, TaskVerifying: true}
