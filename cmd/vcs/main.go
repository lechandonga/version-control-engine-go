// vcs 是本地版本控制引擎的命令行入口。
package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	vcslib "github.com/lechandonga/version-control-engine-go/vcs"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "init":
		r, err := vcslib.Init(cwd)
		if err != nil {
			return err
		}
		fmt.Println("initialized empty repository in", r.Root())
		return nil
	case "help", "-h", "--help":
		usage()
		return nil
	}

	r, err := vcslib.Open(cwd)
	if err != nil {
		return err
	}
	switch cmd {
	case "add":
		if len(rest) == 0 {
			return fmt.Errorf("usage: vcs add <path>...")
		}
		return r.Add(rest)
	case "commit":
		msg := ""
		for i := 0; i < len(rest); i++ {
			if rest[i] == "-m" && i+1 < len(rest) {
				msg = rest[i+1]
				i++
			}
		}
		id, err := r.Commit(vcslib.CommitOptions{
			Message: msg,
			Author:  vcslib.Signature{Name: "user", Email: "user@local"},
			When:    time.Now(),
		})
		if err != nil {
			return err
		}
		fmt.Println(id[:12])
		return nil
	case "status":
		entries, err := r.Status()
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			fmt.Println("clean")
			return nil
		}
		for _, e := range entries {
			fmt.Printf("%s\t%s\n", e.Kind, e.Path)
		}
		return nil
	case "branch":
		if len(rest) == 0 {
			names, cur, err := r.ListBranches()
			if err != nil {
				return err
			}
			for _, n := range names {
				marker := "  "
				if n == cur {
					marker = "* "
				}
				fmt.Println(marker + n)
			}
			return nil
		}
		if strings.HasPrefix(rest[0], "-d") {
			if len(rest) < 2 {
				return fmt.Errorf("usage: vcs branch -d <name>")
			}
			return r.DeleteBranch(rest[1])
		}
		_, err := r.Branch(rest[0])
		return err
	case "checkout", "switch":
		if len(rest) < 1 {
			return fmt.Errorf("usage: vcs checkout <branch>")
		}
		return r.Checkout(rest[0])
	case "merge":
		if len(rest) < 1 {
			return fmt.Errorf("usage: vcs merge <branch>")
		}
		res, err := r.Merge(rest[0], vcslib.CommitOptions{
			Message: "merge " + rest[0],
			Author:  vcslib.Signature{Name: "user", Email: "user@local"},
			When:    time.Now(),
		})
		return printMerge(res, err)
	case "rebase":
		return runRebase(r, rest)
	case "log":
		return printLog(r)
	case "fsck":
		if err := r.Fsck(); err != nil {
			return err
		}
		fmt.Println("ok")
		return nil
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func runRebase(r *vcslib.Repository, rest []string) error {
	author := vcslib.Signature{Name: "user", Email: "user@local"}
	if len(rest) == 0 {
		return fmt.Errorf("usage: vcs rebase <upstream> | --continue | --abort | --recover")
	}
	switch rest[0] {
	case "--continue":
		res, err := r.RebaseContinue(vcslib.RebaseOptions{Author: author, When: time.Now()})
		if err != nil {
			return err
		}
		fmt.Printf("rebase %s: applied=%d skipped=%d\n", res.Kind, res.Applied, res.Skipped)
		return nil
	case "--abort":
		if err := r.RebaseAbort(); err != nil {
			return err
		}
		fmt.Println("rebase aborted")
		return nil
	case "--recover":
		action, err := r.RebaseRecover()
		if err != nil {
			return err
		}
		fmt.Println("recover:", action)
		return nil
	default:
		res, err := r.Rebase(vcslib.RebaseOptions{Upstream: rest[0], Author: author, When: time.Now()})
		if err != nil {
			return printRebase(res, err)
		}
		if res.Kind == "paused" {
			fmt.Printf("rebase paused at conflict in %s\n", res.Conflict)
			fmt.Println("fix conflicts, run `vcs add`, then `vcs rebase --continue`; or `vcs rebase --abort`")
			os.Exit(3)
		}
		return printRebase(res, nil)
	}
}

func printMerge(res *vcslib.MergeResult, err error) error {
	if err != nil {
		var mc *vcslib.MergeConflictError
		if errors.As(err, &mc) {
			fmt.Println("CONFLICT:")
			for _, c := range mc.Conflicts {
				fmt.Printf("  %s\t%s\n", c.Kind, c.Path)
			}
			os.Exit(2)
		}
		return err
	}
	fmt.Printf("merge %s -> %s\n", res.Kind, res.CommitID[:12])
	return nil
}

func printRebase(res *vcslib.RebaseResult, err error) error {
	if err != nil {
		var mc *vcslib.MergeConflictError
		if errors.As(err, &mc) {
			fmt.Println("CONFLICT during rebase:")
			for _, c := range mc.Conflicts {
				fmt.Printf("  %s\t%s\n", c.Kind, c.Path)
			}
			fmt.Println("fix conflicts, run `vcs add`, then `vcs rebase --continue`; or `vcs rebase --abort`")
			os.Exit(2)
		}
		return err
	}
	fmt.Printf("rebase %s: applied=%d skipped=%d head=%s\n",
		res.Kind, res.Applied, res.Skipped, res.Head[:12])
	return nil
}

func printLog(r *vcslib.Repository) error {
	id, err := r.HeadCommit()
	if err != nil {
		return err
	}
	for id != "" {
		c, err := r.ReadCommit(id)
		if err != nil {
			return err
		}
		fmt.Printf("commit %s\n    %s\n", id[:12], c.Message)
		if len(c.Parents) == 0 {
			break
		}
		id = c.Parents[0]
	}
	return nil
}

func usage() {
	fmt.Fprintln(os.Stderr, `vcs - local content-addressable version control engine

usage:
  vcs init
  vcs add <path>...
  vcs commit -m <message>
  vcs status
  vcs branch [<name> | -d <name>]
  vcs checkout <branch>
  vcs merge <branch>
  vcs rebase <upstream> | --continue | --abort | --recover
  vcs log
  vcs fsck`)
}
