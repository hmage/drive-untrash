package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"os"
	"sync"
	"sync/atomic"

	drive "google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/lib/pacer"
	"golang.org/x/net/context"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

var (
	p             *pacer.Pacer
	flagVerbose   = flag.Bool("v", false, "verbose logging")
	countRestored uint64
)

func restoreTrashed(srv *drive.Service, parent string, childs []*drive.File, recurse bool, wg *sync.WaitGroup) {
	// parent is only for logging purposes
	if parent == "" {
		parent = "root"
	}
	if *flagVerbose {
		log.Println("restoring trash in", parent)
	}
	for _, child := range childs {
		if child.ExplicitlyTrashed {
			wg.Add(1)
			go func(child *drive.File) {
				update := drive.File{
					Trashed: false,
				}
				err := p.Call(func() (bool, error) {
					_, err := srv.Files.Update(child.Id, &update).Do()
					return shouldRetry(err)
				})
				if err != nil {
					log.Printf("Failed to restore file %v %v in folder %v: %s", child.Id, child.Name, parent, err)
				} else {
					if *flagVerbose {
						log.Printf("Restored file %v %v in folder %v", child.Id, child.Name, parent)
					}
					atomic.AddUint64(&countRestored, 1)
				}
				wg.Done()
			}(child)
		}

		if recurse && child.MimeType == "application/vnd.google-apps.folder" {
			err := processFolder(srv, child.Id, wg)
			if err != nil {
				log.Println("unable to list", child.Name, err)
				continue
			}
		}
	}
}

func shouldRetry(err error) (bool, error) {
	switch gerr := err.(type) {
	case *googleapi.Error:
		if gerr.Code >= 500 && gerr.Code < 600 {
			// All 5xx errors should be retried
			return true, err
		} else if len(gerr.Errors) > 0 {
			reason := gerr.Errors[0].Reason
			if reason == "rateLimitExceeded" || reason == "userRateLimitExceeded" {
				return true, err
			}
		}
	}
	return false, err
}

func getFolderPage(srv *drive.Service, folderId string, pageToken string) ([]*drive.File, string, error) {
	var (
		fl  *drive.FileList
		err error
	)
	err = p.Call(func() (bool, error) {
		call := srv.Files.List().PageSize(1000).Fields("nextPageToken", "files(id, name, mimeType, explicitlyTrashed)")
		if folderId != "" {
			call.Q(fmt.Sprintf("'%s' in parents and (mimeType = 'application/vnd.google-apps.folder' or trashed = true)", folderId))
		} else {
			call.Q("mimeType = 'application/vnd.google-apps.folder' or trashed = true")
		}
		if pageToken != "" {
			call.PageToken(pageToken)
		}
		fl, err = call.Do()
		return shouldRetry(err)
	})
	if err != nil {
		return nil, "", fmt.Errorf("Unable to retrieve files: %v", err)
	}

	return fl.Files, fl.NextPageToken, nil
}
func processFolder(srv *drive.Service, folderId string, wg *sync.WaitGroup) error {
	var pageToken string
	for {
		var files []*drive.File
		var err error
		files, pageToken, err = getFolderPage(srv, folderId, pageToken)
		if err != nil {
			return fmt.Errorf("Failed to get file listing: %w", err)
		}
		if *flagVerbose {
			log.Println("got page with", len(files), "entries")
		}
		wg.Add(1)
		go func(srv *drive.Service, folderId string, files []*drive.File, wg *sync.WaitGroup) {
			restoreTrashed(srv, folderId, files, true, wg)
			wg.Done()
		}(srv, folderId, files, wg)
		// end of listing, that was last page
		if pageToken == "" {
			break
		}
	}
	return nil
}

// getClient uses a Context and Config to retrieve a Token
// then generate a Client. It returns the generated Client.
func getClient(ctx context.Context, config *oauth2.Config) *http.Client {
	cacheFile, err := tokenCacheFile()
	if err != nil {
		log.Fatalf("Unable to get path to cached credential file. %v", err)
	}
	tok, err := tokenFromFile(cacheFile)
	if err != nil {
		tok = getTokenFromWeb(config)
		saveToken(cacheFile, tok)
	}
	return config.Client(ctx, tok)
}

// getTokenFromWeb uses Config to request a Token.
// It returns the retrieved Token.
func getTokenFromWeb(config *oauth2.Config) *oauth2.Token {
	authURL := config.AuthCodeURL("state-token", oauth2.AccessTypeOffline)
	fmt.Printf("Go to the following link in your browser then type the "+
		"authorization code: \n%v\n", authURL)

	var code string
	if _, err := fmt.Scan(&code); err != nil {
		log.Fatalf("Unable to read authorization code %v", err)
	}

	tok, err := config.Exchange(context.Background(), code)
	if err != nil {
		log.Fatalf("Unable to retrieve token from web %v", err)
	}
	return tok
}

// tokenCacheFile generates credential file path/filename.
// It returns the generated credential path/filename.
func tokenCacheFile() (string, error) {
	return url.QueryEscape("drive-go-quickstart.json"), nil
}

// tokenFromFile retrieves a Token from a given file path.
// It returns the retrieved Token and any read error encountered.
func tokenFromFile(file string) (*oauth2.Token, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	t := &oauth2.Token{}
	err = json.NewDecoder(f).Decode(t)
	defer f.Close()
	return t, err
}

// saveToken uses a file path to create a file and store the
// token in it.
func saveToken(file string, token *oauth2.Token) {
	fmt.Printf("Saving credential file to: %s\n", file)

	data, err := json.Marshal(token)
	if err != nil {
		log.Fatalf("Failed to marshal token into json: %s", err)
	}

	err = ioutil.WriteFile(file, data, 0600)
	if err != nil {
		log.Fatalf("Unable to cache oauth token: %v", err)
	}
}

func main() {
	go func() {
		log.Println(http.ListenAndServe("localhost:6060", nil))
	}()
	fs.Config.LogLevel = fs.LogLevelDebug
	p = pacer.New()
	p.SetCalculator(pacer.NewGoogleDrive())
	p.SetRetries(5)
	p.SetMaxConnections(10)
	ctx := context.Background()

	flag.Parse()

	b, err := ioutil.ReadFile("client_secret.json")
	if err != nil {
		log.Fatalf("Unable to read client secret file: %v", err)
	}

	// If modifying these scopes, delete your previously saved credentials
	config, err := google.ConfigFromJSON(b, "https://www.googleapis.com/auth/drive")
	if err != nil {
		log.Fatalf("Unable to parse client secret file to config: %v", err)
	}
	client := getClient(ctx, config)

	srv, err := drive.New(client)
	if err != nil {
		log.Fatalf("Unable to retrieve drive Client %v", err)
	}

	wg := sync.WaitGroup{}

	if args := flag.Args(); len(args) > 0 {
		for _, folderId := range args {
			err := processFolder(srv, folderId, &wg)
			if err != nil {
				log.Printf("Unable to list folder %q: %v", folderId, err)
			}
		}
	} else {
		err := processFolder(srv, "", &wg)
		if err != nil {
			log.Fatalf("Unable to list drive: %v", err)
		}
	}

	log.Printf("Waiting for goroutines to finish...")
	wg.Wait()
	log.Printf("Restored %d files in total", countRestored)
}
