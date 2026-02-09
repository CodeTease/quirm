package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
)

func main() {
	if len(os.Args) < 4 {
		fmt.Println("Usage: sign_url <secret> <path> <query_params>")
		fmt.Println("Example: sign_url mysecret /images/logo.png 'w=200&h=100'")
		os.Exit(1)
	}

	secret := os.Args[1]
	path := os.Args[2]
	queryStr := os.Args[3]

	params, err := url.ParseQuery(queryStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing query: %v\n", err)
		os.Exit(1)
	}

	// Logic from pkg/handlers/http.go validateSignature
	keys := make([]string, 0, len(params))
	for k := range params {
		if k == "s" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString(path)
	if len(keys) > 0 {
		b.WriteString("?")
	}
	for i, k := range keys {
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(params.Get(k))
		if i < len(keys)-1 {
			b.WriteString("&")
		}
	}

	toSign := b.String()

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(toSign))
	signature := hex.EncodeToString(mac.Sum(nil))

	fmt.Print(signature)
}
