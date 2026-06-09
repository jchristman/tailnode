package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// resolvePreauthKey returns a Tailscale auth key. Priority: --preauthkey flag,
// AUTH_KEY env, TS_AUTHKEY env (tsnet default), then the env file.
func resolvePreauthKey(flagValue, envFile string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if v := os.Getenv("AUTH_KEY"); v != "" {
		return v, nil
	}
	if v := os.Getenv("TS_AUTHKEY"); v != "" {
		return v, nil
	}
	if envFile == "" {
		return "", fmt.Errorf("set AUTH_KEY in .env, or pass --preauthkey")
	}
	vals, err := loadDotEnv(envFile)
	if err != nil {
		return "", err
	}
	if v := vals["AUTH_KEY"]; v != "" {
		return v, nil
	}
	if v := vals["TS_AUTHKEY"]; v != "" {
		return v, nil
	}
	return "", fmt.Errorf("%s: no AUTH_KEY or TS_AUTHKEY", envFile)
}

// resolveControlURL returns a custom control server URL. Priority: --login-server
// flag, LOGIN_SERVER env, CONTROL_URL env, TS_CONTROL_URL env, then the env file.
// An empty return value means use the Tailscale default.
func resolveControlURL(flagValue, envFile string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	for _, key := range []string{"LOGIN_SERVER", "CONTROL_URL", "TS_CONTROL_URL"} {
		if v := os.Getenv(key); v != "" {
			return v, nil
		}
	}
	if envFile == "" {
		return "", nil
	}
	if _, err := os.Stat(envFile); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	vals, err := loadDotEnv(envFile)
	if err != nil {
		return "", err
	}
	for _, key := range []string{"LOGIN_SERVER", "CONTROL_URL", "TS_CONTROL_URL"} {
		if v := vals[key]; v != "" {
			return v, nil
		}
	}
	return "", nil
}

func loadDotEnv(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%s: file not found", path)
		}
		return nil, err
	}
	defer f.Close()

	vals := make(map[string]string)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		val = strings.Trim(val, `"'`)
		vals[key] = val
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return vals, nil
}
