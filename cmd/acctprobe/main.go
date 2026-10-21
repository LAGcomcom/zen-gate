package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"zen-gate/internal/qoderhub/remote"
)

// acctprobe asks each pooled account, one at a time, for a one-line answer and
// prints what the upstream said. It exists to answer "which account is really
// out of quota" without toggling the live pool.
func main() {
	src, err := remote.DefaultAccountsDir()
	if err != nil {
		panic(err)
	}
	idxData, err := os.ReadFile(filepath.Join(src, "index.json"))
	if err != nil {
		panic(err)
	}
	var idx struct {
		Accounts []struct {
			ID      string `json:"id"`
			Label   string `json:"label"`
			Enabled bool   `json:"enabled"`
			UID     string `json:"uid"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(idxData, &idx); err != nil {
		panic(err)
	}

	proxy, _ := remote.ProxySource("")
	base := remote.ResolveBaseURL("")
	model := os.Getenv("QPROBE_MODEL")
	if model == "" {
		model = "Qwen3.8-Flash"
	}
	reps := 1
	if v := os.Getenv("QPROBE_N"); v != "" {
		fmt.Sscanf(v, "%d", &reps)
	}
	fmt.Printf("base=%s proxy=%q model=%s reps=%d\n", base, proxy, model, reps)

	for _, a := range idx.Accounts {
		if len(os.Args) > 1 && os.Args[1] != a.ID {
			continue
		}
		tmp, err := os.MkdirTemp("", "acctprobe")
		if err != nil {
			panic(err)
		}
		credData, err := os.ReadFile(filepath.Join(src, a.ID+".json"))
		if err != nil {
			fmt.Printf("%s: read cred: %v\n", a.Label, err)
			continue
		}
		if mid := os.Getenv("QPROBE_MACHINE"); mid != "" {
			var doc map[string]any
			if json.Unmarshal(credData, &doc) == nil {
				if auth, ok := doc["auth"].(map[string]any); ok {
					auth["machine_id"] = mid
					if b, err := json.Marshal(doc); err == nil {
						credData = b
					}
				}
			}
		}
		if err := os.WriteFile(filepath.Join(tmp, a.ID+".json"), credData, 0o600); err != nil {
			panic(err)
		}
		one := map[string]any{"version": 1, "accounts": []map[string]any{{
			"id": a.ID, "label": a.Label, "uid": a.UID, "enabled": true,
		}}}
		blob, _ := json.Marshal(one)
		if err := os.WriteFile(filepath.Join(tmp, "index.json"), blob, 0o600); err != nil {
			panic(err)
		}
		pool := remote.NewPool(tmp)
		if err := pool.Load(); err != nil {
			panic(err)
		}
		client := remote.New(remote.Config{BaseURL: base, ProxyURL: proxy, Timeout: 60 * time.Second, Pool: pool})
		if q, qerr := client.FetchQuota(context.Background()); qerr != nil {
			fmt.Printf("    quota: %v\n", qerr)
		} else {
			reset := "n/a"
			if q.ResetAtMS > 0 {
				reset = time.UnixMilli(q.ResetAtMS).Format("01-02 15:04")
			}
			fmt.Printf("    quota: exceeded=%v remaining=%.2f/%.2f %s reset=%s type=%s\n",
				q.IsExceeded, q.Remaining, q.Total, q.Unit, reset, q.UserType)
		}
		start := time.Now()
		for rep := 0; rep < reps; rep++ {
			res, err := client.Chat(context.Background(), remote.ChatRequest{
				Model:     model,
				Messages:  []remote.Message{{Role: "user", Content: "say hi"}},
				MaxTokens: 8,
			}, nil)
			if err != nil {
				fmt.Printf("%-18s %-20s FAIL %5dms  %v\n", a.ID[:14], a.Label, time.Since(start).Milliseconds(), err)
			} else {
				fmt.Printf("%-18s %-20s OK   %5dms  %q\n", a.ID[:14], a.Label, time.Since(start).Milliseconds(), res.Text)
			}
			start = time.Now()
		}
		_ = os.RemoveAll(tmp)
	}
}
