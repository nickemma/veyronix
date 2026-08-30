package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/nickemma/plinth/internal/manifest"
	"github.com/nickemma/plinth/internal/state"
)

const defaultAPI = "http://localhost:8080"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	apiURL := os.Getenv("PLINTH_API")
	if apiURL == "" {
		apiURL = defaultAPI
	}
	var err error
	switch os.Args[1] {
	case "up":
		err = up(apiURL, os.Args[2:])
	case "status":
		err = status(apiURL, os.Args[2:])
	case "logs":
		err = collection(apiURL, os.Args[2:], "logs")
	case "rollback":
		err = action(apiURL, os.Args[2:], "rollback")
	case "pause", "resume", "destroy":
		err = action(apiURL, os.Args[2:], os.Args[1])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "plinth:", err)
		os.Exit(1)
	}
}

func up(apiURL string, args []string) error {
	flags := flag.NewFlagSet("up", flag.ContinueOnError)
	file := flags.String("f", "plinth.yaml", "manifest path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	m, err := manifest.LoadFile(*file)
	if err != nil {
		return err
	}
	var service state.Service
	if err := requestJSON(apiURL+"/api/v1/services", http.MethodPost, m, &service); err != nil {
		return err
	}
	printJSON(service)
	if service.Phase == state.PhaseFailed || service.Phase == state.PhaseRolledBack {
		return errors.New(service.Message)
	}
	return nil
}

func status(apiURL string, args []string) error {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	watch := flags.Bool("watch", false, "poll until a terminal state")
	if err := flags.Parse(args); err != nil {
		return err
	}
	name := ""
	if flags.NArg() > 0 {
		name = flags.Arg(0)
	}
	path := "/api/v1/services"
	if name != "" {
		path += "/" + name
	}
	for {
		var value any
		if err := requestJSON(apiURL+path, http.MethodGet, nil, &value); err != nil {
			return err
		}
		printJSON(value)
		if !*watch || name == "" {
			return nil
		}
		var service state.Service
		encoded, _ := json.Marshal(value)
		_ = json.Unmarshal(encoded, &service)
		switch service.Phase {
		case state.PhaseReady, state.PhaseRolledBack, state.PhaseFailed, state.PhasePaused, state.PhaseDestroyed:
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func collection(apiURL string, args []string, collection string) error {
	if len(args) == 0 {
		return errors.New(collection + " requires a service name")
	}
	var value any
	if err := requestJSON(apiURL+"/api/v1/services/"+args[0]+"/"+collection, http.MethodGet, nil, &value); err != nil {
		return err
	}
	return printJSON(value)
}

func action(apiURL string, args []string, action string) error {
	if len(args) == 0 {
		return errors.New(action + " requires a service name")
	}
	var value map[string]any
	body := map[string]int{}
	if action == "rollback" && len(args) > 1 {
		var revision int
		if _, err := fmt.Sscanf(args[1], "%d", &revision); err != nil {
			return fmt.Errorf("invalid revision %q", args[1])
		}
		body["revision"] = revision
	}
	if err := requestJSON(apiURL+"/api/v1/services/"+args[0]+"/"+action, http.MethodPost, body, &value); err != nil {
		return err
	}
	return printJSON(value)
}

func requestJSON(url, method string, body any, target any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", url, err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if response.StatusCode >= 300 {
		return fmt.Errorf("%s: %s", response.Status, strings.TrimSpace(string(data)))
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func printJSON(value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: plinth {up|status|logs|rollback|pause|resume|destroy} [arguments]")
}
