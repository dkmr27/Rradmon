package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"

	"github.com/dkmr27/Rradmon/internal/store"
)

const minPasswordLen = 10

// runAdmin manages the admin password:
//
//	rradmon admin -db rradmon.db set-password    (prompts twice, or reads one line from a pipe)
//	rradmon admin -db rradmon.db clear-password  (turns admin login off)
func runAdmin(args []string) error {
	fs := flag.NewFlagSet("admin", flag.ExitOnError)
	dbPath := fs.String("db", "rradmon.db", "database file")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: rradmon admin [-db file] set-password | clear-password")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("admin: give one action")
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	switch fs.Arg(0) {
	case "set-password":
		pw, err := readNewPassword(os.Stdin)
		if err != nil {
			return err
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
		if err != nil {
			return err
		}
		if err := st.SetAdminPasswordHash(string(hash)); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "Admin password set. A running rradmon uses it from the next login; no restart needed.")
		return nil
	case "clear-password":
		if err := st.SetAdminPasswordHash(""); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "Admin password removed; admin login is off.")
		return nil
	}
	fs.Usage()
	return fmt.Errorf("admin: unknown action %q", fs.Arg(0))
}

// readNewPassword prompts twice without echo on a terminal; from a pipe it
// reads one line (for scripted setups).
func readNewPassword(in *os.File) (string, error) {
	fd := int(in.Fd())
	if !term.IsTerminal(fd) {
		line, err := bufio.NewReader(in).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		return checkPassword(strings.TrimRight(line, "\r\n"))
	}
	fmt.Fprint(os.Stderr, "New admin password: ")
	a, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	fmt.Fprint(os.Stderr, "Again: ")
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	if string(a) != string(b) {
		return "", errors.New("the passwords do not match")
	}
	return checkPassword(string(a))
}

func checkPassword(pw string) (string, error) {
	if len([]rune(pw)) < minPasswordLen {
		return "", fmt.Errorf("use at least %d characters", minPasswordLen)
	}
	if len(pw) > 72 {
		return "", errors.New("use at most 72 bytes (bcrypt's limit)")
	}
	return pw, nil
}
