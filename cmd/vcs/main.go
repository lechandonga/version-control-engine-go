package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/lechandonga/version-control-engine-go/vcs"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	var err error
	switch cmd {
	case "init":
		_, err = vcs.Init(".")
	case "add":
		err = withRepo(func(r *vcs.Repo) error {
			if len(args) == 0 {
				return errors.New("usage: vcs add <path>")
			}
			return r.Add(args[0])
		})
	case "commit":
		err = commitCmd(args)
	case "branch":
		err = branchCmd(args)
	case "checkout", "co":
		err = withRepo(func(r *vcs.Repo) error { return r.Checkout(args[0]) })
	case "merge":
		err = mergeCmd(args)
	case "resolve":
		err = resolveCmd(args, false)
	case "rebase":
		err = rebaseCmd(args)
	case "rebase-resolve":
		err = resolveCmd(args, true)
	case "log":
		err = logCmd()
	case "reflog":
		err = reflogCmd(args)
	case "restore":
		err = restoreCmd(args)
	case "pack":
		err = withRepo(func(r *vcs.Repo) error {
			st, err := r.Pack()
			if err != nil {
				return err
			}
			if st.Reused {
				fmt.Println("pack: nothing new to archive (idempotent)")
			} else {
				fmt.Printf("packed %d objects into %s, loose left: %d\n",
					st.Objects, st.PackFile, st.LooseFiles)
			}
			return nil
		})
	case "gc":
		err = gcCmd(args)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", classify(err))
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`vcs - 本地版本控制引擎（含归档 / 回收 / 操作留痕）

用法:
  vcs init
  vcs add <path> | vcs commit -m <msg> [--allow-empty]
  vcs branch (list|create <name> [<commit>]|delete <name>)
  vcs checkout <branch|commit>
  vcs merge <branch> | vcs merge abort | vcs resolve <path>
  vcs rebase <branch> | vcs rebase continue|abort|skip | vcs rebase-resolve <path>
  vcs log
  vcs reflog [--ref <name>] [--since RFC3339] [--until RFC3339] [--check]
  vcs restore --line <n> [--ref <name>]
  vcs pack
  vcs gc [--dry-run] [--retain 336h]
`)
}

func withRepo(f func(*vcs.Repo) error) error {
	r, err := vcs.Open(".")
	if err != nil {
		return err
	}
	return f(r)
}

func commitCmd(args []string) error {
	fs := flag.NewFlagSet("commit", flag.ContinueOnError)
	msg := fs.String("m", "", "commit message")
	empty := fs.Bool("allow-empty", false, "allow empty commit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return withRepo(func(r *vcs.Repo) error {
		id, err := r.Commit(vcs.CommitOptions{Message: *msg, AllowEmpty: *empty})
		if err != nil {
			return err
		}
		fmt.Println(id)
		return nil
	})
}

func branchCmd(args []string) error {
	if len(args) == 0 || args[0] == "list" {
		return withRepo(func(r *vcs.Repo) error {
			names, err := r.Branches()
			if err != nil {
				return err
			}
			cur, detached, err := r.CurrentBranch()
			if err != nil {
				return err
			}
			for _, n := range names {
				switch {
				case !detached && n == cur:
					fmt.Println("*", n)
				default:
					fmt.Println("  ", n)
				}
			}
			return nil
		})
	}
	switch args[0] {
	case "create":
		if len(args) < 2 {
			return errors.New("usage: vcs branch create <name> [<commit>]")
		}
		return withRepo(func(r *vcs.Repo) error {
			start := ""
			if len(args) >= 3 {
				start = args[2]
			} else {
				head, err := r.HEADCommit()
				if err != nil {
					return err
				}
				start = head
			}
			return r.CreateBranch(args[1], start)
		})
	case "delete", "rm":
		return withRepo(func(r *vcs.Repo) error { return r.DeleteBranch(args[1]) })
	default:
		return errors.New("usage: vcs branch list|create|delete")
	}
}

func mergeCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: vcs merge <branch>|abort")
	}
	switch args[0] {
	case "abort":
		return withRepo(func(r *vcs.Repo) error { return r.AbortMerge() })
	default:
		return withRepo(func(r *vcs.Repo) error {
			res, err := r.Merge(args[0], "")
			if err != nil {
				return err
			}
			fmt.Println(res.Mode, res.CommitID)
			return nil
		})
	}
}

func resolveCmd(args []string, rebase bool) error {
	if len(args) < 1 {
		return errors.New("usage: resolve <path>  (resolved content read from stdin)")
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	return withRepo(func(r *vcs.Repo) error {
		if rebase {
			return r.ResolveRebaseConflict(args[0], data)
		}
		return r.ResolveConflict(args[0], data)
	})
}

func rebaseCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: vcs rebase <branch>|continue|abort|skip")
	}
	return withRepo(func(r *vcs.Repo) error {
		switch args[0] {
		case "continue":
			return r.ContinueRebase()
		case "abort":
			return r.AbortRebase()
		case "skip":
			return r.SkipRebaseCommit()
		default:
			return r.Rebase(args[0])
		}
	})
}

func logCmd() error {
	return withRepo(func(r *vcs.Repo) error {
		head, err := r.HEADCommit()
		if err != nil {
			return err
		}
		for head != "" {
			c, err := r.ReadCommit(head)
			if err != nil {
				return err
			}
			fmt.Printf("%s  %s  %s\n", head[:12], c.Timestamp.Format(time.RFC3339), c.Message)
			if len(c.Parents) == 0 {
				break
			}
			head = c.Parents[0]
		}
		return nil
	})
}

func reflogCmd(args []string) error {
	fs := flag.NewFlagSet("reflog", flag.ContinueOnError)
	ref := fs.String("ref", "", "filter by ref")
	since := fs.String("since", "", "RFC3339")
	until := fs.String("until", "", "RFC3339")
	check := fs.Bool("check", false, "health check only")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return withRepo(func(r *vcs.Repo) error {
		if *check {
			h, err := r.CheckReflog()
			if err != nil {
				return err
			}
			fmt.Printf("entries=%d corrupt=%d\n", h.Entries, len(h.Corrupt))
			for _, c := range h.Corrupt {
				fmt.Printf("  line %d: %s\n", c.Line, c.Reason)
			}
			return nil
		}
		var s, u time.Time
		var err error
		if *since != "" {
			s, err = time.Parse(time.RFC3339, *since)
			if err != nil {
				return err
			}
		}
		if *until != "" {
			u, err = time.Parse(time.RFC3339, *until)
			if err != nil {
				return err
			}
		}
		entries, err := r.ReadReflog(*ref, s, u)
		if err != nil {
			return err
		}
		for _, e := range entries {
			fmt.Printf("L%-5d %s\n", e.Line, e.String())
		}
		return nil
	})
}

func restoreCmd(args []string) error {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	line := fs.Int("line", 0, "reflog line number to restore from")
	ref := fs.String("ref", "", "ref to filter by")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *line <= 0 {
		return errors.New("restore requires --line")
	}
	return withRepo(func(r *vcs.Repo) error {
		entries, err := r.ReadReflog(*ref, time.Time{}, time.Time{})
		if err != nil {
			return err
		}
		for _, e := range entries {
			if e.Line == *line {
				return r.RestoreRef(e, "")
			}
		}
		return fmt.Errorf("reflog line %d not found", *line)
	})
}

func gcCmd(args []string) error {
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "preview only")
	retain := fs.Duration("retain", vcs.DefaultRetention, "retention for new objects")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return withRepo(func(r *vcs.Repo) error {
		opts := vcs.GCOptions{Retain: *retain}
		var rep *vcs.GCReport
		var err error
		if *dryRun {
			rep, err = r.PreviewGC(opts)
		} else {
			rep, err = r.GC(opts)
		}
		if err != nil {
			return err
		}
		fmt.Printf("reachable=%d unreachable=%d reclaimable=%d too-young=%d loose-deleted=%d packs-rewritten=%d\n",
			rep.Reachable, rep.Unreachable, len(rep.Reclaimable), len(rep.TooYoung),
			rep.LooseDeleted, rep.PacksRewritten)
		fmt.Println("roots:")
		for kind, ids := range rep.Roots {
			fmt.Printf("  %s: %d\n", kind, len(ids))
		}
		if *dryRun {
			for _, id := range rep.Reclaimable {
				fmt.Println("  drop", id)
			}
		}
		return nil
	})
}

// classify 把分类错误翻译为带固定前缀的单行输出，方便自动化判定。
func classify(err error) string {
	var (
		missing *vcs.ObjectMissing
		trunc   *vcs.ObjectTruncated
		tamper  *vcs.ObjectTampered
		corrupt *vcs.ObjectCorrupt
		refNF   *vcs.RefNotFound
		refBad  *vcs.RefCorrupt
		over    *vcs.WouldOverwrite
		mc      *vcs.MergeConflict
		rc      *vcs.RebaseConflict
		unrel   *vcs.UnrelatedHistories
	)
	switch {
	case errors.As(err, &missing):
		return "MISSING: " + err.Error()
	case errors.As(err, &trunc):
		return "TRUNCATED: " + err.Error()
	case errors.As(err, &tamper):
		return "TAMPERED: " + err.Error()
	case errors.As(err, &corrupt):
		return "CORRUPT: " + err.Error()
	case errors.As(err, &refNF):
		return "REF_NOT_FOUND: " + err.Error()
	case errors.As(err, &refBad):
		return "REF_CORRUPT: " + err.Error()
	case errors.As(err, &over):
		return "WOULD_OVERWRITE: " + err.Error()
	case errors.As(err, &mc):
		return "MERGE_CONFLICT: " + err.Error()
	case errors.As(err, &rc):
		return "REBASE_CONFLICT: " + err.Error()
	case errors.As(err, &unrel):
		return "UNRELATED: " + err.Error()
	default:
		return err.Error()
	}
}
