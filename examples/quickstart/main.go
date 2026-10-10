// Quickstart connects the Go SDK to a running flagpost server, evaluates a
// flag for a few users, and prints every change pushed over the stream.
//
//	FLAGPOST_URL=http://localhost:8080 FLAGPOST_SDK_KEY=fp-sdk-... go run ./examples/quickstart
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/J-X0-cloud/flagpost/eval"
	"github.com/J-X0-cloud/flagpost/model"
	"github.com/J-X0-cloud/flagpost/sdk"
)

func main() {
	url, key := os.Getenv("FLAGPOST_URL"), os.Getenv("FLAGPOST_SDK_KEY")
	if url == "" || key == "" {
		log.Fatal("set FLAGPOST_URL and FLAGPOST_SDK_KEY")
	}
	flag := "new-checkout"
	if len(os.Args) > 1 {
		flag = os.Args[1]
	}

	users := []eval.Context{
		eval.NewContext("user-1").With("country", "NZ").With("plan", "pro"),
		eval.NewContext("user-2").With("country", "US").With("plan", "free"),
		eval.NewContext("user-3").With("country", "DE").With("plan", "pro"),
	}

	var client *sdk.Client
	report := func(*model.Snapshot) {
		if client == nil {
			return
		}
		for _, u := range users {
			res := client.Evaluate(flag, u)
			fmt.Printf("%s %-7s -> %-8s %s\n", time.Now().Format("15:04:05"), u.Key, string(res.Value), res.Reason)
		}
	}
	var err error
	client, err = sdk.New(sdk.Config{BaseURL: url, SDKKey: key, OnUpdate: report, FlushInterval: 5 * time.Second})
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.WaitReady(ctx); err != nil {
		log.Fatal(err)
	}
	report(nil)
	fmt.Println("watching for changes; press Ctrl+C to exit")

	stop, done := signal.NotifyContext(context.Background(), os.Interrupt)
	defer done()
	<-stop.Done()
}
