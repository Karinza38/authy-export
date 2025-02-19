package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/alexzorin/authy"
	"golang.org/x/crypto/ssh/terminal"
)

// We'll persist this to the filesystem so we don't need to
// re-register the device every time
type deviceRegistration struct {
	UserID   uint64 `json:"user_id,omitempty"`
	DeviceID uint64 `json:"device_id,omitempty"`
	Seed     string `json:"seed,omitempty"`
	APIKey   string `json:"api_key,omitempty"`
}

func main() {
	//savePtr := flag.String("save", "", "Save encrypted tokens to this JSON file")
	flag.Parse()

	if len(os.Args) != 2 {
		fmt.Println("Usage: ./authy-export-darwin-amd64 <export_file> ")
		return
	}
	exportFile, err := resolvePath(os.Args[1])
	if err != nil {
		fmt.Println("Error resolving exportFile path:", err)
		return
	}
	f, err := os.OpenFile(exportFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Println(err)
	}
	defer f.Close()

	var resp struct {
		Tokens authy.AuthenticatorTokensResponse `json:"tokens"`
		Apps   authy.AuthenticatorAppsResponse   `json:"apps"`
	}

	// Get tokens from the server
	// If we don't already have a registered device, prompt the user for one
	regr, err := loadExistingDeviceRegistration()
	if err == nil {
		fmt.Println("Found existing device registration")
	} else if os.IsNotExist(err) {
		fmt.Println("No existing device registration found, will perform registration now")
		regr, err = newInteractiveDeviceRegistration()
		if err != nil {
			log.Fatalf("Device registration failed: %v", err)
		}
	} else if err != nil {
		log.Fatalf("Could not check for existing device registration: %v", err)
	}

	// By now we have a valid user and device ID
	fmt.Printf("Authy User ID %d, Device ID %d\n", regr.UserID, regr.DeviceID)

	cl, err := authy.NewClient()
	if err != nil {
		log.Fatalf("Couldn't create API client: %v", err)
	}

	// Fetch the apps
	resp.Apps, err = cl.QueryAuthenticatorApps(nil, regr.UserID, regr.DeviceID, regr.Seed)
	if err != nil {
		log.Fatalf("Could not fetch authenticator apps: %v", err)
	}
	if !resp.Apps.Success {
		log.Fatalf("Failed to fetch authenticator apps: %+v", resp.Apps)
	}

	// Fetch the actual tokens now
	resp.Tokens, err = cl.QueryAuthenticatorTokens(nil, regr.UserID, regr.DeviceID, regr.Seed)
	if err != nil {
		log.Fatalf("Could not fetch authenticator tokens: %v", err)
	}
	if !resp.Tokens.Success {
		log.Fatalf("Failed to fetch authenticator tokens: %+v", resp.Tokens)
	}

	// Display decrypted tokens to the terminal
	// We'll need the prompt the user to give the decryption password
	pp := []byte(os.Getenv("AUTHY_EXPORT_PASSWORD"))
	if len(pp) == 0 {
		fmt.Printf("Please provide your Authy TOTP backup password: ")
		var err error
		pp, err = terminal.ReadPassword(int(os.Stdin.Fd()))
		if err != nil {
			log.Fatalf("Failed to read the password: %v", err)
		}
	}

	// Print out in https://github.com/google/google-authenticator/wiki/Key-Uri-Format format
	if len(resp.Tokens.AuthenticatorTokens) > 0 {
		fmt.Printf("Found %d Regular Auth tokens\n", len(resp.Tokens.AuthenticatorTokens))

		for _, tok := range resp.Tokens.AuthenticatorTokens {
			decrypted, err := tok.Decrypt(string(pp))
			if err != nil {
				log.Printf("Failed to decrypt token %s: %v", tok.Description(), err)
				continue
			}

			params := url.Values{}
			params.Set("secret", decrypted)
			params.Set("digits", strconv.Itoa(tok.Digits))
			u := url.URL{
				Scheme:   "otpauth",
				Host:     "totp",
				Path:     tok.Description(),
				RawQuery: params.Encode(),
			}
			//fmt.Println(u.String())
			// write u.String() to exportFile
			if _, err := f.WriteString(u.String() + "\n"); err != nil {
				log.Println(err)

			}
		}
	}
	if len(resp.Apps.AuthenticatorApps) > 0 {
		fmt.Printf("Found %d Authy Specific tokens\n", len(resp.Apps.AuthenticatorApps))
		for _, app := range resp.Apps.AuthenticatorApps {
			tok, err := app.Token()
			if err != nil {
				log.Printf("Failed to decode app %s: %v", app.Name, err)
				continue
			}
			params := url.Values{}
			params.Set("secret", tok)
			params.Set("digits", strconv.Itoa(app.Digits))
			params.Set("period", "10")
			u := url.URL{
				Scheme:   "otpauth",
				Host:     "totp",
				Path:     app.Name,
				RawQuery: params.Encode(),
			}
			//fmt.Println(u.String())
			if _, err := f.WriteString(u.String() + "\n"); err != nil {
				log.Println(err)

			}
		}
	}
}

func newInteractiveDeviceRegistration() (deviceRegistration, error) {
	var regr deviceRegistration
	// The first part of device registration requires the user's phone number that
	// is attached to their Authy account
	var phoneCC int
	var phoneNumber string

	var err error
	sc := bufio.NewScanner(os.Stdin)
	fmt.Print("\nWhat is your phone number's country code? (digits only): ")
	if !sc.Scan() {
		return regr, errors.New("please provide a phone country code, e.g. 61")
	}
	if phoneCC, err = strconv.Atoi(strings.TrimSpace(sc.Text())); err != nil {
		return regr, err
	}
	fmt.Print("\nWhat is your phone number? (digits only): ")
	if !sc.Scan() {
		return regr, errors.New("please provide a phone country code, e.g. 12341234")
	}
	phoneNumber = strings.TrimSpace(sc.Text())
	if err := sc.Err(); err != nil {
		return regr, err
	}

	// Query the existence of the Authy account
	cl, err := authy.NewClient()
	if err != nil {
		return regr, nil
	}
	userStatus, err := cl.QueryUser(nil, phoneCC, phoneNumber)
	if err != nil {
		return regr, err
	}
	if !userStatus.IsActiveUser() {
		return regr, errors.New("There doesn't seem to be an Authy account attached to that phone number")
	}

	// Begin a device registration using Authy app push notification
	regStart, err := cl.RequestDeviceRegistration(nil, userStatus.AuthyID, authy.ViaMethodPush)
	if err != nil {
		return regr, err
	}
	if !regStart.Success {
		return regr, fmt.Errorf("Authy did not accept the device registration request: %+v", regStart)
	}

	// Poll for a while until the user has responded to the device registration
	var regPIN string
	timeout := time.Now().Add(5 * time.Minute)
	for {
		if timeout.Before(time.Now()) {
			return regr, errors.New("gave up waiting for user to respond to Authy device registration request")
		}

		log.Printf("Checking device registration status (%s until we give up)", time.Until(timeout).Truncate(time.Second))

		regStatus, err := cl.CheckDeviceRegistration(nil, userStatus.AuthyID, regStart.RequestID)
		if err != nil {
			return regr, err
		}
		if regStatus.Status == "accepted" {
			regPIN = regStatus.PIN
			break
		} else if regStatus.Status != "pending" {
			return regr, fmt.Errorf("invalid status while waiting for device registration: %s", regStatus.Status)
		}

		time.Sleep(5 * time.Second)
	}

	// We have the registration PIN, complete the registration
	regComplete, err := cl.CompleteDeviceRegistration(nil, userStatus.AuthyID, regPIN)
	if err != nil {
		return regr, err
	}
	if regComplete.Device.SecretSeed == "" {
		return regr, errors.New("Something went wrong completing the device registration")
	}

	regr.UserID = regComplete.AuthyID
	regr.DeviceID = regComplete.Device.ID
	regr.Seed = regComplete.Device.SecretSeed
	regr.APIKey = regComplete.Device.APIKey

	// We have everything we need, persist it to disk
	regrPath, err := configPath()
	if err != nil {
		return regr, err
	}
	f, err := os.OpenFile(regrPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return regr, err
	}
	defer f.Close()
	if err := json.NewEncoder(f).Encode(regr); err != nil {
		return regr, err
	}

	return regr, nil
}

func loadExistingDeviceRegistration() (deviceRegistration, error) {
	regrPath, err := configPath()
	if err != nil {
		return deviceRegistration{}, err
	}

	f, err := os.Open(regrPath)
	if err != nil {
		return deviceRegistration{}, err
	}
	defer f.Close()

	var regr deviceRegistration
	return regr, json.NewDecoder(f).Decode(&regr)
}

func configPath() (string, error) {
	regrPath, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(regrPath, "authy-go.json"), nil
}

func resolvePath(path string) (string, error) {
	if path[:2] != "~/" {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return home + path[1:], nil
}
