// Copyright 2017-2018 the u-root Authors. All rights reserved
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package syslinux implements a syslinux config file parser.
//
// See http://www.syslinux.org/wiki/index.php?title=Config for general syslinux
// config features.
//
// Currently, only the APPEND, INCLUDE, KERNEL, LABEL, DEFAULT, and INITRD
// directives are partially supported.
package syslinux

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/u-root/u-root/pkg/boot"
	"github.com/u-root/u-root/pkg/curl"
	"github.com/u-root/u-root/pkg/uio"
)

func probeIsolinuxFiles() []string {
	files := make([]string, 0, 10)
	// search order from the syslinux wiki
	// http://wiki.syslinux.org/wiki/index.php?title=Config
	// TODO: do we want to handle extlinux too ?
	dirs := []string{
		"boot/isolinux",
		"isolinux",
		"boot/syslinux",
		"syslinux",
		"",
	}
	confs := []string{
		"isolinux.cfg",
		"syslinux.cfg",
	}
	for _, dir := range dirs {
		for _, conf := range confs {
			if dir == "" {
				files = append(files, conf)
			} else {
				files = append(files, filepath.Join(dir, conf))
			}
		}
	}
	return files
}

// ParseLocalConfig treats diskDir like a mount point on the local file system
// and finds an isolinux config under there.
func ParseLocalConfig(ctx context.Context, diskDir string) ([]boot.OSImage, error) {
	for _, relname := range probeIsolinuxFiles() {
		dir, name := filepath.Split(relname)

		// "When booting, the initial working directory for SYSLINUX /
		// ISOLINUX will be the directory containing the initial
		// configuration file."
		//
		// https://wiki.syslinux.org/wiki/index.php?title=Config#Working_directory
		wd := &url.URL{
			Scheme: "file",
			Path:   filepath.Join(diskDir, dir),
		}

		imgs, err := ParseConfigFile(ctx, curl.DefaultSchemes, name, wd)
		if curl.IsURLError(err) {
			continue
		}
		return imgs, err
	}
	return nil, fmt.Errorf("no valid syslinux config found on %s", diskDir)
}

// ParseConfigFile parses a Syslinux configuration as specified in
// http://www.syslinux.org/wiki/index.php?title=Config
//
// Currently, only the APPEND, INCLUDE, KERNEL, LABEL, DEFAULT, and INITRD
// directives are partially supported.
//
// `s` is used to fetch any files that must be parsed or provided.
//
// `wd` is the default scheme, host, and path for any files named as a
// relative path - e.g. kernel, include, and initramfs paths are requested
// relative to the wd.
func ParseConfigFile(ctx context.Context, s curl.Schemes, url string, wd *url.URL) ([]boot.OSImage, error) {
	p := newParser(wd, s)
	if err := p.appendFile(ctx, url); err != nil {
		return nil, err
	}

	// Intended order:
	//
	// 1. nerfDefaultEntry
	// 2. defaultEntry
	// 3. labels in order they appeared in config
	if len(p.labelOrder) == 0 {
		return nil, nil
	}
	if len(p.defaultEntry) > 0 {
		p.labelOrder = append([]string{p.defaultEntry}, p.labelOrder...)
	}
	if len(p.nerfDefaultEntry) > 0 {
		p.labelOrder = append([]string{p.nerfDefaultEntry}, p.labelOrder...)
	}
	p.labelOrder = dedupStrings(p.labelOrder)

	var images []boot.OSImage
	for _, label := range p.labelOrder {
		if img, ok := p.linuxEntries[label]; ok {
			images = append(images, img)
		}
	}
	return images, nil
}

func dedupStrings(list []string) []string {
	var newList []string
	seen := make(map[string]struct{})
	for _, s := range list {
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			newList = append(newList, s)
		}
	}
	return newList
}

type parser struct {
	// linuxEntries is a map of label name -> label configuration.
	linuxEntries map[string]*boot.LinuxImage

	// labelOrder is the order of label entries in linuxEntries.
	labelOrder []string

	defaultEntry     string
	nerfDefaultEntry string

	// parser internals.
	globalAppend string
	scope        scope
	curEntry     string
	wd           *url.URL
	schemes      curl.Schemes
}

type scope uint8

const (
	scopeGlobal scope = iota
	scopeEntry
)

// newParser returns a new PXE parser using working directory `wd`
// and schemes `s`.
//
// If a path encountered in a configuration file is relative instead of a full
// URL, `wd` is used as the "working directory" of that relative path; the
// resulting URL is roughly `wd.String()/path`.
//
// `s` is used to get files referred to by URLs.
func newParser(wd *url.URL, s curl.Schemes) *parser {
	return &parser{
		linuxEntries: make(map[string]*boot.LinuxImage),
		scope:        scopeGlobal,
		wd:           wd,
		schemes:      s,
	}
}

func parseURL(surl string, wd *url.URL) (*url.URL, error) {
	u, err := url.Parse(surl)
	if err != nil {
		return nil, fmt.Errorf("could not parse URL %q: %v", surl, err)
	}

	if len(u.Scheme) == 0 && wd != nil {
		u.Scheme = wd.Scheme

		if len(u.Host) == 0 {
			// If this is not there, it was likely just a path.
			u.Host = wd.Host
			u.Path = filepath.Join(wd.Path, filepath.Clean(u.Path))
		}
	}
	return u, nil
}

// getFile parses `url` relative to the config's working directory and returns
// an io.Reader for the requested url.
//
// If url is just a relative path and not a full URL, c.wd is used as the
// "working directory" of that relative path; the resulting URL is roughly
// path.Join(wd.String(), url).
func (c *parser) getFile(url string) (io.ReaderAt, error) {
	u, err := parseURL(url, c.wd)
	if err != nil {
		return nil, err
	}

	return c.schemes.LazyFetch(u)
}

// appendFile parses the config file downloaded from `url` and adds it to `c`.
func (c *parser) appendFile(ctx context.Context, url string) error {
	u, err := parseURL(url, c.wd)
	if err != nil {
		return err
	}

	r, err := c.schemes.Fetch(ctx, u)
	if err != nil {
		return err
	}
	config, err := uio.ReadAll(r)
	if err != nil {
		return err
	}
	log.Printf("Got config file %s:\n%s\n", r, string(config))
	return c.append(ctx, string(config))
}

// Append parses `config` and adds the respective configuration to `c`.
func (c *parser) append(ctx context.Context, config string) error {
	// Here's a shitty parser.
	for _, line := range strings.Split(config, "\n") {
		// This is stupid. There should be a FieldsN(...).
		kv := strings.Fields(line)
		if len(kv) <= 1 {
			continue
		}
		directive := strings.ToLower(kv[0])
		var arg string
		if len(kv) == 2 {
			arg = kv[1]
		} else {
			arg = strings.Join(kv[1:], " ")
		}

		switch directive {
		case "default":
			c.defaultEntry = arg

		case "nerfdefault":
			c.nerfDefaultEntry = arg

		case "include":
			if err := c.appendFile(ctx, arg); curl.IsURLError(err) {
				log.Printf("failed to parse %s: %v", arg, err)
				// Means we didn't find the file. Just ignore
				// it.
				// TODO(hugelgupf): plumb a logger through here.
				continue
			} else if err != nil {
				return err
			}

		case "menu":
			opt := strings.Fields(arg)
			if len(opt) < 1 {
				continue
			}
			switch strings.ToLower(opt[0]) {
			case "label":
				// Note that "menu label" only changes the
				// displayed label, not the identifier for this
				// entry.
				if e, ok := c.linuxEntries[c.curEntry]; ok && len(opt) > 1 {
					e.Name = strings.Join(opt[1:], " ")
				}

			case "default":
				// Are we in label scope?
				//
				// "Only valid after a LABEL statement" -syslinux wiki.
				if c.scope == scopeEntry {
					c.defaultEntry = c.curEntry
				}
			}

		case "label":
			// We forever enter label scope.
			c.scope = scopeEntry
			c.curEntry = arg
			c.linuxEntries[c.curEntry] = &boot.LinuxImage{
				Cmdline: c.globalAppend,
				Name:    c.curEntry,
			}
			c.labelOrder = append(c.labelOrder, c.curEntry)

		case "kernel", "linux":
			if e, ok := c.linuxEntries[c.curEntry]; ok {
				k, err := c.getFile(arg)
				if err != nil {
					return err
				}
				e.Kernel = k
			}

		case "initrd":
			if e, ok := c.linuxEntries[c.curEntry]; ok {
				// TODO: support multiple comma-separated initrds.
				// TODO: append "initrd=$arg" to the cmdline.
				//
				// For how this interacts with global appends,
				// read
				// https://wiki.syslinux.org/wiki/index.php?title=Directives/append
				i, err := c.getFile(arg)
				if err != nil {
					return err
				}
				e.Initrd = i
			}

		case "append":
			switch c.scope {
			case scopeGlobal:
				c.globalAppend = arg

			case scopeEntry:
				if arg == "-" {
					c.linuxEntries[c.curEntry].Cmdline = ""
				} else {
					// Yes, we explicitly _override_, not
					// concatenate. If a specific append
					// directive is present, a global
					// append directive is ignored.
					//
					// Also, "If you enter multiple APPEND
					// statements in a single LABEL entry,
					// only the last one will be used".
					//
					// https://wiki.syslinux.org/wiki/index.php?title=Directives/append
					c.linuxEntries[c.curEntry].Cmdline = arg
				}
			}
		}
	}

	// Go through all labels and download the initrds.
	for _, label := range c.linuxEntries {
		// If the initrd was set via the INITRD directive, don't
		// overwrite that.
		//
		// TODO(hugelgupf): Is this really what syslinux does? Does
		// INITRD trump cmdline? Does it trump global? What if both the
		// directive and cmdline initrd= are set? Does it depend on the
		// order in the config file? (My current best guess: order.)
		//
		// Answer: Normally, the INITRD directive appends to the
		// cmdline, and the _last_ effective initrd= parameter is used
		// for loading initrd files.
		if label.Initrd != nil {
			continue
		}

		for _, opt := range strings.Fields(label.Cmdline) {
			optkv := strings.Split(opt, "=")
			if optkv[0] != "initrd" {
				continue
			}

			i, err := c.getFile(optkv[1])
			if err != nil {
				return err
			}
			label.Initrd = i
		}
	}
	return nil

}
