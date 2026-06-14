package config

import (
    "context"
	"bufio"
	"log"
    "strconv"
    "strings"
	"os"

    "github.com/quangtrieu1312/tmasqued/constants"
)

func Load(ctx *context.Context) {
    configPath := constants.CONF_PATH
    file, err := os.Open(configPath)
    if err != nil {
        log.Fatalf("Failed to open config file %v: %v", configPath, err)
        os.Exit(1)
    }
    defer file.Close()
    scanner := bufio.NewScanner(file)
    for scanner.Scan() {
        line := strings.TrimSpace(scanner.Text())
        if len(line) == 0 || strings.HasPrefix(line, "#") {
            continue
        }
        parts := strings.SplitN(line, "=", 2)
        if len(parts) != 2 {
            continue
        }
        key := strings.TrimSpace(parts[0])
        value := strings.TrimSpace(parts[1])
        if key == "" {
            continue
        }
        *ctx = context.WithValue(*ctx, key, value)
    }
}

// Bool reads a config key from the context as a boolean, returning def when the key
// is absent/empty or unparseable. Accepts the strconv.ParseBool forms (1/t/true,
// 0/f/false, ...).
func Bool(ctx context.Context, key string, def bool) bool {
	if v, ok := ctx.Value(key).(string); ok && v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

// Int reads a config key from the context as an int, returning def when the key is
// absent/empty or unparseable.
func Int(ctx context.Context, key string, def int) int {
	if v, ok := ctx.Value(key).(string); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
