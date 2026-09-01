package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/cockroachdb/errors"

	"gogs.io/gogs/internal/conf"
	"gogs.io/gogs/internal/database"
)

type result struct {
	UserID   int64  `json:"user_id"`
	Username string `json:"username"`
	Token    string `json:"token"`
}

func main() {
	if err := run(); err != nil {
		if _, writeErr := fmt.Fprintln(os.Stderr, err); writeErr != nil {
			os.Exit(1)
		}
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "", "Path to the Gogs configuration file.")
	username := flag.String("username", "", "Username to create.")
	password := flag.String("password", "", "Password for the new user.")
	email := flag.String("email", "", "Email address for the new user.")
	fullName := flag.String("full-name", "", "Full name for the new user.")
	tokenName := flag.String("token-name", "smoke", "Name for the personal access token.")
	flag.Parse()

	if *configPath == "" || *username == "" || *password == "" || *email == "" {
		return errors.New("config, username, password, and email are required")
	}
	if err := conf.Init(*configPath); err != nil {
		return errors.Wrap(err, "initialize Gogs configuration")
	}
	conf.InitLogging(true)

	if err := database.NewEngine(); err != nil {
		return errors.Wrap(err, "initialize Gogs database")
	}

	user, err := database.Handle.Users().Create(
		context.Background(),
		*username,
		*email,
		database.CreateUserOptions{
			Password:  *password,
			FullName:  *fullName,
			Activated: true,
		},
	)
	if err != nil {
		return errors.Wrap(err, "create Gogs smoke user")
	}
	token, err := database.Handle.AccessTokens().Create(context.Background(), user.ID, *tokenName)
	if err != nil {
		return errors.Wrap(err, "create Gogs smoke personal access token")
	}

	if err := json.NewEncoder(os.Stdout).Encode(result{
		UserID:   user.ID,
		Username: user.Name,
		Token:    token.Sha1,
	}); err != nil {
		return errors.Wrap(err, "write bootstrap result")
	}
	return nil
}
