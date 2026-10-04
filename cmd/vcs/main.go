package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	vcs "github.com/lechandonga/version-control-engine-go/vcs"
)

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "vcs: "+format+"\n", args...)
	os.Exit(1)
}

func openRepo() *vcs.Repo {
	dir, err := os.Getwd()
	if err != nil {
		fatalf("%v", err)
	}
	r, err := vcs.Open(dir)
	if err != nil {
		fatalf("%v", err)
	}
	return r
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println(`用法: vcs <命令> [参数]
  init                          初始化仓库
  commit -m <msg>               提交工作区快照
  branch <name>                 创建分支
  unbranch <name>               删除分支（当前所在分支拒绝；留痕，可 recover 找回）
  switch <name|commit>          切换分支或检出提交
  merge <name>                  合并目标分支
  merge -abort                  放弃在途合并
  rebase <name>                 把当前分支重放到目标之上
  rebase -continue|-abort       继续 / 回退在途重放
  reflog [-ref <name>]          查看操作留痕（默认全部，按时间排序）
  recover <ref> <commit>        按记录把引用恢复到指定位置（支持记录里的短标识）
  pack                          把松散对象归档为包文件
  gc [-dry-run] [-grace 24h]    回收不可达对象`)
		return
	}
	switch os.Args[1] {
	case "init":
		dir, _ := os.Getwd()
		if _, err := vcs.Init(dir); err != nil {
			fatalf("%v", err)
		}
		fmt.Println("initialized repository in", dir)
	case "commit":
		fs := flag.NewFlagSet("commit", flag.ExitOnError)
		msg := fs.String("m", "", "commit message")
		fs.Parse(os.Args[2:])
		id, err := openRepo().Commit(*msg)
		if err != nil {
			fatalf("%v", err)
		}
		fmt.Println("committed", id)
	case "branch":
		if err := openRepo().CreateBranch(os.Args[2]); err != nil {
			fatalf("%v", err)
		}
	case "unbranch":
		if err := openRepo().DeleteBranch(os.Args[2]); err != nil {
			fatalf("%v", err)
		}
	case "switch":
		if err := openRepo().Checkout(os.Args[2]); err != nil {
			fatalf("%v", err)
		}
	case "merge":
		fs := flag.NewFlagSet("merge", flag.ExitOnError)
		abort := fs.Bool("abort", false, "abort in-progress merge")
		fs.Parse(os.Args[2:])
		r := openRepo()
		if *abort {
			if err := r.MergeAbort(); err != nil {
				fatalf("%v", err)
			}
			return
		}
		id, err := r.Merge(fs.Arg(0))
		if err != nil {
			fatalf("%v", err)
		}
		fmt.Println("merged at", id)
	case "rebase":
		fs := flag.NewFlagSet("rebase", flag.ExitOnError)
		cont := fs.Bool("continue", false, "continue in-progress rebase")
		abort := fs.Bool("abort", false, "abort in-progress rebase")
		fs.Parse(os.Args[2:])
		r := openRepo()
		var err error
		switch {
		case *cont:
			err = r.RebaseContinue()
		case *abort:
			err = r.RebaseAbort()
		default:
			err = r.Rebase(fs.Arg(0))
		}
		if err != nil {
			fatalf("%v", err)
		}
	case "reflog":
		fs := flag.NewFlagSet("reflog", flag.ExitOnError)
		ref := fs.String("ref", "", "只查看指定引用的记录")
		fs.Parse(os.Args[2:])
		r := openRepo()
		var entries []vcs.ReflogEntry
		var corrupt []vcs.ReflogCorrupt
		var err error
		if *ref != "" {
			entries, corrupt, err = r.ReadReflog(*ref)
		} else {
			entries, corrupt, err = r.ReadAllReflog()
		}
		if err != nil {
			fatalf("%v", err)
		}
		for _, e := range entries {
			fmt.Printf("%s  %-14s %-20s %s -> %s  %s\n",
				e.Time.Format(time.RFC3339), e.Op, e.Ref, short(e.Old), short(e.New), e.Msg)
		}
		for _, c := range corrupt {
			fmt.Fprintf(os.Stderr, "warning: %v\n", &c)
		}
	case "recover":
		if len(os.Args) < 4 {
			fatalf("usage: vcs recover <ref> <commit>")
		}
		if err := openRepo().Recover(os.Args[2], os.Args[3]); err != nil {
			fatalf("%v", err)
		}
		fmt.Println("recovered", os.Args[2], "to", os.Args[3])
	case "pack":
		res, err := openRepo().Pack()
		if err != nil {
			fatalf("%v", err)
		}
		fmt.Printf("packed %d objects into %s (skipped %d corrupt)\n", res.Packed, res.PackFile, len(res.Skipped))
	case "gc":
		fs := flag.NewFlagSet("gc", flag.ExitOnError)
		dry := fs.Bool("dry-run", false, "只预览，不删除")
		grace := fs.Duration("grace", 24*time.Hour, "保留期")
		fs.Parse(os.Args[2:])
		res, err := openRepo().GC(vcs.GCOptions{DryRun: *dry, Grace: *grace})
		if err != nil {
			fatalf("%v", err)
		}
		verb := "removed"
		if *dry {
			verb = "would remove"
		}
		fmt.Printf("%s %d objects, kept %d by grace, %d reachable loose\n",
			verb, len(res.Removed), len(res.Kept), res.Reachable)
		for _, id := range res.Removed {
			fmt.Println("  ", id)
		}
	default:
		fatalf("unknown command %q", os.Args[1])
	}
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	if id == "" {
		return "-"
	}
	return id
}
