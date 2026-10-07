// stalebloom 把目录下所有 .bloom 文件头部 8 字节时间戳改写为 N 天前
// 用于端到端复现 issue#5：低频 feed 的去重状态时间戳自然老化超过 30 天后，
// 旧版本加载时会清零重推，修复后应保留记忆
//
// 用法: stalebloom -dir <rss2telegram-data 目录> -days 35
package main

import (
	"encoding/binary"
	"flag"
	"log"
	"os"
	"path/filepath"
	"time"
)

func main() {
	dir := flag.String("dir", ".", "bloom 文件所在目录")
	days := flag.Int("days", 35, "回退天数")
	flag.Parse()

	stale := make([]byte, 8)
	binary.LittleEndian.PutUint64(stale, uint64(time.Now().AddDate(0, 0, -*days).UnixNano()))

	files, err := filepath.Glob(filepath.Join(*dir, "*.bloom"))
	if err != nil {
		log.Fatalf("glob: %v", err)
	}
	if len(files) == 0 {
		log.Fatalf("no .bloom files in %s", *dir)
	}

	for _, f := range files {
		func() {
			fh, err := os.OpenFile(f, os.O_RDWR, 0644)
			if err != nil {
				log.Fatalf("open %s: %v", f, err)
			}
			defer fh.Close()
			if _, err := fh.WriteAt(stale, 0); err != nil {
				log.Fatalf("write %s: %v", f, err)
			}
		}()
		log.Printf("staled %s (-%dd)", f, *days)
	}
}
