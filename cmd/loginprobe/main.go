package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/yg/mimo2api/internal/config"
	"github.com/yg/mimo2api/internal/mimo"
)

func main() {
	cfg, _ := config.Load()
	c := mimo.New(mimo.Options{
		Base:      cfg.UpstreamBase(),
		UserAgent: cfg.UserAgent(),
		SecChUA:   cfg.SecChUA(),
		Timezone:  cfg.Timezone(),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if os.Getenv("DUMP") == "1" {
		dumpRawLogin(ctx, c, os.Args[1], os.Args[2])
		return
	}

	sess, err := c.Login(ctx, os.Args[1], os.Args[2])
	if err != nil {
		fmt.Println("LOGIN-ERROR:", err)
		os.Exit(1)
	}
	fmt.Printf("LOGIN-OK userId=%s\n", sess.UserID)
	if err := c.Validate(ctx, sess.Cookie); err != nil {
		fmt.Println("VALIDATE-ERROR:", err)
		os.Exit(1)
	}
	fmt.Println("VALIDATE-OK")
}
