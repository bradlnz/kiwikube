package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type config struct {
	ClusterSlots   [5]string `json:"cluster_slots"`
	ArchiveEnabled bool      `json:"archive_enabled"`
	ArchiveDir     string    `json:"archive_dir"`
	ArchiveStreams int       `json:"archive_streams"`
	Theme          string    `json:"theme"`
	RefreshSeconds int       `json:"refresh_seconds"`
	HideSidebar    bool      `json:"hide_sidebar"`
	SidebarWidth   int       `json:"sidebar_width"`
	LogLines       int       `json:"log_lines"`
	Namespace      string    `json:"namespace"`
	Shell          string    `json:"shell"`
	SSHUser        string    `json:"ssh_user"`
}

func defaultConfig() config {
	return config{ArchiveEnabled: true, ArchiveDir: archiveDirectory(), ArchiveStreams: 32, Theme: "kiwi", RefreshSeconds: 5, SidebarWidth: 26, LogLines: 2000, Shell: "/bin/sh"}
}
func configPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "kiwikube.json"
	}
	return filepath.Join(dir, "kiwikube", "config.json")
}
func loadConfig(path string) (config, error) {
	c := defaultConfig()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("config %s: %w", path, err)
	}
	return c, c.validate()
}
func (c config) validate() error {
	if c.ArchiveStreams < 1 || c.ArchiveStreams > 512 {
		return fmt.Errorf("archive_streams must be 1–512")
	}
	if c.ArchiveEnabled && c.ArchiveDir == "" {
		return fmt.Errorf("archive_dir must not be empty")
	}
	if c.Theme != "kiwi" && c.Theme != "ocean" && c.Theme != "amber" {
		return fmt.Errorf("theme must be kiwi, ocean, or amber")
	}
	if c.RefreshSeconds < 2 || c.RefreshSeconds > 300 {
		return fmt.Errorf("refresh_seconds must be 2–300")
	}
	if c.SidebarWidth < 18 || c.SidebarWidth > 50 {
		return fmt.Errorf("sidebar_width must be 18–50")
	}
	if c.LogLines < 100 || c.LogLines > 10000 {
		return fmt.Errorf("log_lines must be 100–10000")
	}
	if c.Shell == "" {
		return fmt.Errorf("shell must name an executable, for example /bin/sh")
	}
	return nil
}
func (c config) save(path string) error {
	if err := c.validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".kiwikube-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(append(data, '\n')); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
