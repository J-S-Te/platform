package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

type row struct {
	FileID       string `gorm:"column:file_id"`
	RelativePath string `gorm:"column:relative_path"`
	SizeBytes    uint64 `gorm:"column:size_bytes"`
	SHA256       []byte `gorm:"column:sha256"`
}

func main() {
	limit := flag.Int("limit", 1000, "maximum READY versions to inspect")
	interval := flag.Duration("interval", 20*time.Millisecond, "delay between files")
	flag.Parse()
	root, dsn := os.Getenv("FILE_GATEWAY_STORAGE_ROOT"), os.Getenv("FILE_GATEWAY_DATABASE_DSN")
	if *limit < 1 || *limit > 100000 || *interval < 0 || root == "" || dsn == "" {
		log.Fatal("valid root, DSN, limit and interval are required")
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		log.Fatal(err)
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatal(err)
	}
	var rows []row
	err = db.Table("file_version AS v").Select("v.file_id, v.storage_relative_path AS relative_path, v.size_bytes, v.sha256").Joins("JOIN file_object AS f ON f.id = v.file_id").Where("f.status = ? AND v.status = ?", "READY", "READY").Order("v.file_id, v.version_no").Limit(*limit).Scan(&rows).Error
	if err != nil {
		log.Fatal(err)
	}
	missing, mismatched := 0, 0
	var total uint64
	for _, item := range rows {
		clean := filepath.Clean(filepath.FromSlash(item.RelativePath))
		if clean == "." || filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
			mismatched++
			continue
		}
		path := filepath.Join(realRoot, clean)
		real, realErr := filepath.EvalSymlinks(path)
		if realErr != nil || (real != realRoot && !strings.HasPrefix(real, realRoot+string(os.PathSeparator))) {
			missing++
			continue
		}
		file, openErr := os.Open(real)
		if openErr != nil {
			missing++
			continue
		}
		hash := sha256.New()
		size, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil || size < 0 {
			mismatched++
			continue
		}
		total += uint64(size)
		if uint64(size) != item.SizeBytes || !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), hex.EncodeToString(item.SHA256)) {
			mismatched++
		}
		if *interval > 0 {
			time.Sleep(*interval)
		}
	}
	fmt.Printf("file inventory: inspected=%d bytes=%d missing=%d mismatched=%d\n", len(rows), total, missing, mismatched)
	if missing > 0 || mismatched > 0 {
		os.Exit(2)
	}
}
