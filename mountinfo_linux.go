//go:build linux

package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type mountInfo struct {
	ID           int
	ParentID     int
	MajorMinor   string
	Root         string
	MountPoint   string
	MountOptions []string
	Optional     []string
	Filesystem   string
	Source       string
	SuperOptions []string
}

func readMountInfo(path string) ([]mountInfo, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read mount table %s: %w", path, err)
	}
	defer file.Close()

	var mounts []mountInfo
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		mount, err := parseMountInfoLine(scanner.Text())
		if err != nil {
			return nil, fmt.Errorf("parse mount table line %d: %w", lineNumber, err)
		}
		mounts = append(mounts, mount)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read mount table %s: %w", path, err)
	}
	return mounts, nil
}

func parseMountInfoLine(line string) (mountInfo, error) {
	fields := strings.Fields(line)
	separator := -1
	for i, field := range fields {
		if field == "-" {
			separator = i
			break
		}
	}
	if separator < 6 || len(fields) < separator+4 {
		return mountInfo{}, fmt.Errorf("malformed entry")
	}
	id, err := strconv.Atoi(fields[0])
	if err != nil {
		return mountInfo{}, fmt.Errorf("invalid mount ID %q", fields[0])
	}
	parentID, err := strconv.Atoi(fields[1])
	if err != nil {
		return mountInfo{}, fmt.Errorf("invalid parent mount ID %q", fields[1])
	}
	root, err := unescapeMountField(fields[3])
	if err != nil {
		return mountInfo{}, fmt.Errorf("invalid root path: %w", err)
	}
	mountPoint, err := unescapeMountField(fields[4])
	if err != nil {
		return mountInfo{}, fmt.Errorf("invalid mount path: %w", err)
	}
	source, err := unescapeMountField(fields[separator+2])
	if err != nil {
		return mountInfo{}, fmt.Errorf("invalid source: %w", err)
	}
	superOptions, err := unescapeMountField(fields[separator+3])
	if err != nil {
		return mountInfo{}, fmt.Errorf("invalid super options: %w", err)
	}
	optional := make([]string, separator-6)
	copy(optional, fields[6:separator])
	return mountInfo{
		ID:           id,
		ParentID:     parentID,
		MajorMinor:   fields[2],
		Root:         root,
		MountPoint:   mountPoint,
		MountOptions: strings.Split(fields[5], ","),
		Optional:     optional,
		Filesystem:   fields[separator+1],
		Source:       source,
		SuperOptions: strings.Split(superOptions, ","),
	}, nil
}

func unescapeMountField(value string) (string, error) {
	var out strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] != '\\' {
			out.WriteByte(value[i])
			continue
		}
		if i+3 >= len(value) {
			return "", fmt.Errorf("truncated escape in %q", value)
		}
		for _, digit := range value[i+1 : i+4] {
			if digit < '0' || digit > '7' {
				return "", fmt.Errorf("invalid octal escape in %q", value)
			}
		}
		decoded, err := strconv.ParseUint(value[i+1:i+4], 8, 8)
		if err != nil {
			return "", fmt.Errorf("invalid octal escape in %q: %w", value, err)
		}
		out.WriteByte(byte(decoded))
		i += 3
	}
	return out.String(), nil
}
