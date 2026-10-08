package agents

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"zen-gate/internal/store"
)

// backupFile stores a pre-injection copy of an agent config under
// <zen-gate home>/backups/<agent>/.
func backupFile(agentID, path string, data []byte) (string, error) {
	dir := filepath.Join(store.Home(), "backups", agentID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	base := filepath.Base(path)
	stamp := time.Now().Format("20060102-150405.000")
	// A restore point is only useful if the next one does not land on it, and
	// latestBackup reads these names back in order — so the sequence number has
	// to sort after the timestamp, always present, always the same width.
	for i := 0; ; i++ {
		p := filepath.Join(dir, fmt.Sprintf("%s-%03d-%s", stamp, i, base))
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		_, werr := f.Write(data)
		cerr := f.Close()
		if werr != nil {
			return p, werr
		}
		return p, cerr
	}
}

// backupBeforeWrite copies an existing config aside immediately before an
// adapter rewrites the whole document. Missing files are not backed up, and a
// failed copy never blocks the write.
func backupBeforeWrite(agentID, path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	_, _ = backupFile(agentID, path, data)
}

// latestBackup returns the newest backup of a given base filename, if any.
func latestBackup(agentID, base string) string {
	dir := filepath.Join(store.Home(), "backups", agentID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	names := []string{}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), base) {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return ""
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	return filepath.Join(dir, names[0])
}
