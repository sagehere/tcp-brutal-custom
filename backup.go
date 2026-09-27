//go:build linux

package main

import (
	"archive/zip"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func backup() (string, error) {
	if os.Geteuid() != 0 {
		return "", errors.New("root required")
	}
	dir := filepath.Join(dataDir, "backups")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	name := fmt.Sprintf("backup-%s.zip", time.Now().UTC().Format("20060102T150405Z"))
	target := filepath.Join(dir, name)
	snapshot := filepath.Join(dir, ".history-snapshot.db")
	os.Remove(snapshot)
	db, err := sql.Open("sqlite", filepath.Join(dataDir, "history.db"))
	if err != nil {
		return "", err
	}
	_, err = db.Exec("VACUUM INTO ?", snapshot)
	db.Close()
	if err != nil {
		return "", err
	}
	defer os.Remove(snapshot)
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	zw := zip.NewWriter(out)
	add := func(source, name string) error {
		file, err := os.Open(source)
		if err != nil {
			return err
		}
		defer file.Close()
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = io.Copy(w, file)
		return err
	}
	if err = add(configPath(), "config.json"); err == nil {
		err = add(snapshot, "history.db")
	}
	if e := zw.Close(); err == nil {
		err = e
	}
	if e := out.Close(); err == nil {
		err = e
	}
	if err != nil {
		os.Remove(target)
		return "", err
	}
	return target, nil
}

func restore(name string) error {
	if os.Geteuid() != 0 {
		return errors.New("root required")
	}
	if filepath.Base(name) != name || !strings.HasPrefix(name, "backup-") || !strings.HasSuffix(name, ".zip") {
		return errors.New("use a backup filename from the local backups directory")
	}
	path := filepath.Join(dataDir, "backups", name)
	z, err := zip.OpenReader(path)
	if err != nil {
		return err
	}
	defer z.Close()
	temp, err := os.MkdirTemp(dataDir, "restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	found := map[string]bool{}
	for _, f := range z.File {
		if f.Name != "config.json" && f.Name != "history.db" {
			return errors.New("unexpected archive entry")
		}
		if found[f.Name] || f.UncompressedSize64 > 1<<30 {
			return errors.New("invalid backup")
		}
		found[f.Name] = true
		in, e := f.Open()
		if e != nil {
			return e
		}
		out, e := os.OpenFile(filepath.Join(temp, f.Name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			in.Close()
			return e
		}
		written, e := io.Copy(out, io.LimitReader(in, int64(f.UncompressedSize64)+1))
		if e == nil && written != int64(f.UncompressedSize64) {
			e = errors.New("invalid backup entry size")
		}
		out.Close()
		if closeErr := in.Close(); e == nil {
			e = closeErr
		}
		if e != nil {
			return e
		}
	}
	if !found["config.json"] || !found["history.db"] {
		return errors.New("incomplete backup")
	}
	// Validate both files before changing the live installation.
	b, err := os.ReadFile(filepath.Join(temp, "config.json"))
	if err != nil {
		return err
	}
	var cfg config
	if err = json.Unmarshal(b, &cfg); err != nil {
		return err
	}
	if cfg.WebPort == 0 || cfg.PasswordHash == "" {
		return errors.New("invalid backup configuration")
	}
	db, err := sql.Open("sqlite", filepath.Join(temp, "history.db"))
	if err != nil {
		return err
	}
	var integrity string
	err = db.QueryRow("PRAGMA integrity_check").Scan(&integrity)
	db.Close()
	if err != nil || integrity != "ok" {
		return errors.New("invalid history database")
	}
	services := []string{"tcp-brutal-custom-manager.service", "tcp-brutal-custom-web.service"}
	if out, e := exec.Command("systemctl", append([]string{"stop"}, services...)...).CombinedOutput(); e != nil {
		return fmt.Errorf("stop services: %s: %w", out, e)
	}
	defer exec.Command("systemctl", append([]string{"start"}, services...)...).Run()
	currentDB := filepath.Join(dataDir, "history.db")
	oldConfig := configPath() + ".restore-old"
	oldDB := currentDB + ".restore-old"
	if err = os.Rename(configPath(), oldConfig); err != nil {
		return err
	}
	if err = os.Rename(currentDB, oldDB); err != nil {
		os.Rename(oldConfig, configPath())
		return err
	}
	os.Remove(currentDB + "-wal")
	os.Remove(currentDB + "-shm")
	if err = os.Rename(filepath.Join(temp, "config.json"), configPath()); err == nil {
		err = os.Rename(filepath.Join(temp, "history.db"), currentDB)
	}
	if err == nil {
		out, e := exec.Command("systemctl", append([]string{"start"}, services...)...).CombinedOutput()
		if e != nil {
			err = fmt.Errorf("start restored services: %s: %w", out, e)
		}
	}
	if err != nil {
		exec.Command("systemctl", append([]string{"stop"}, services...)...).Run()
		os.Remove(configPath())
		os.Remove(currentDB)
		os.Rename(oldConfig, configPath())
		os.Rename(oldDB, currentDB)
		return err
	}
	os.Remove(oldConfig)
	os.Remove(oldDB)
	return nil
}
