package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "net/http/pprof"
)

type bytes int

func (b bytes) String() string {
	bf := float64(b)
	for _, unit := range []string{"", "K", "M", "G", "T", "P", "E", "Z"} {
		if math.Abs(bf) < 1000.0 {
			return fmt.Sprintf("%3.1f %sB", bf, unit)
		}
		bf /= 1000.0
	}
	return fmt.Sprintf("%.1fYiB", bf)
}

type FileDescription struct {
	IsPrimary     string `json:"isPrimary"`
	Name          string `json:"name"`
	Url           string `json:"url"`
	Size          string `json:"size"`
	Version       string `json:"version"`
	DatePublished string `json:"datePublished"`
}

type application struct {
	errorLog *log.Logger
	infoLog  *log.Logger
}

func (app *application) Scrape(url string) ([]FileDescription, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	status := fmt.Sprintf("%s %s %s", resp.Request.Method, resp.Request.URL, resp.Status)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s", status)
	}
	app.infoLog.Print(status)

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	s := string(b)
	targetString := string("\"downloadFile\":")
	a := strings.Index(s, targetString)
	if a == -1 {
		return nil, fmt.Errorf("failed to find %s field in response body", targetString)
	}

	app.infoLog.Printf("%s was found in responce body", targetString)

	s1 := string(b[a:])
	targetRune := ']'
	a1 := strings.IndexRune(s1, targetRune)
	if a1 == -1 {
		return nil, fmt.Errorf("%s is invalid json array. Failed to find %c after %s", targetString, targetRune, targetString)
	}

	app.infoLog.Printf("%s is valid json array", targetString)

	files := []FileDescription{}
	err = json.Unmarshal(b[a+len(targetString):a+a1+1], &files)
	if err != nil {
		return nil, err
	}

	return files, nil
}

func dumpVersion(downloadDir string, version string) (string, error) {
	filePath := downloadDir + string(os.PathSeparator) + "version.txt"
	f, err := os.Create(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	_, err = f.Write([]byte(version))
	if err != nil {
		return "", err
	}

	return filePath, nil
}

func downloadFile(client *http.Client, url, downloadDir, fileName string) error {

	filePath := filepath.Join(downloadDir, fileName)
	output, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("create %s: %w", filePath, err)
	}
	defer output.Close()

	res, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s failed: %s", url, res.Status)
	}

	if _, err := io.Copy(output, res.Body); err != nil {
		return fmt.Errorf("write %s: %w", fileName, err)
	}

	return nil
}

func main() {
	dir := flag.String("dir", ".", "download dir")
	url := flag.String("url", "https://aka.ms/mipsdkbins", "url for scraping")
	timeout := flag.Int("timeout", 600, "downloading timeout per file in seconds")
	isVersionOnlyMode := flag.Bool("version-only", false, "no downloading actually happens, returns mipsdk binaries version if found")
	cpuprofile := flag.String("cpuprofile", "", "write cpu profile to file")
	memprofile := flag.String("memprofile", "", "write memory profile to file")

	flag.Parse()

	app := &application{
		infoLog:  log.New(os.Stdout, "INFO\t", log.Ldate|log.Ltime),
		errorLog: log.New(os.Stderr, "ERROR\t", log.Ldate|log.Ltime|log.Lshortfile),
	}

	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			app.errorLog.Fatal(err)
		}
		defer f.Close()

		if err := pprof.StartCPUProfile(f); err != nil {
			app.errorLog.Fatal(err)
		}
		defer pprof.StopCPUProfile()
	}

	downloadDir, err := filepath.Abs(*dir)
	if err != nil {
		app.errorLog.Fatal(err)
	}

	err = os.MkdirAll(downloadDir, 0755)
	if err != nil {
		app.errorLog.Fatal(err)
	}

	app.infoLog.Printf("version only mode: %v", *isVersionOnlyMode)
	app.infoLog.Printf("download dir: %v", downloadDir)
	app.infoLog.Printf("url for scraping: %s", *url)
	if !*isVersionOnlyMode {
		app.infoLog.Printf("downloading timeout per file in seconds: %v", *timeout)
	}

	files, err := app.Scrape(*url)
	if err != nil {
		app.errorLog.Fatal(err)
	}

	version := "unknown"
	if len(files) > 0 {
		version = files[0].Version
	}
	app.infoLog.Printf("Data scraping is successful. MIP SDK binaries version %s. Found %v items", version, len(files))

	for i, file := range files {
		app.infoLog.Printf("%s (%v out of %v items)", file.Url, i+1, len(files))
	}

	if *isVersionOnlyMode {
		versionPath, err := dumpVersion(downloadDir, version)
		if err != nil {
			app.errorLog.Fatal(err)
		}

		app.infoLog.Printf("%s is created successfully", versionPath)
	} else {
		transport := &http.Transport{
			MaxIdleConns:        len(files),
			MaxIdleConnsPerHost: len(files),
			MaxConnsPerHost:     0,
			DisableCompression:  true,
		}

		client := &http.Client{
			Transport: transport,
			Timeout:   time.Duration(*timeout) * time.Second,
		}

		// files = files[28:31]
		var wg sync.WaitGroup
		wg.Add(len(files))

		errors := make(chan error, len(files))

		var counter int64
		for _, file := range files {
			go func(file FileDescription, errors chan error) {
				defer wg.Done()

				if err := downloadFile(client, file.Url, downloadDir, file.Name); err != nil {
					errors <- err
					return
				}

				size, err := strconv.Atoi(file.Size)
				if err != nil {
					errors <- fmt.Errorf("Error while converting %s to int", file.Size)
					return
				}

				atomic.AddInt64(&counter, 1)
				app.infoLog.Printf("%s is downloaded successfully: %v: %v out %v items", file.Name, bytes(size), atomic.LoadInt64(&counter), len(files))
			}(file, errors)
		}
		wg.Wait()
		close(errors)

		var downloadFailed bool

		for err := range errors {
			if err != nil {
				downloadFailed = true
				app.errorLog.Print(err)
			}
		}

		if downloadFailed {
			app.errorLog.Fatal()
		}

		if *memprofile != "" {
			f, err := os.Create(*memprofile)
			if err != nil {
				app.errorLog.Fatal(err)
			}
			defer f.Close()

			if err := pprof.WriteHeapProfile(f); err != nil {
				app.errorLog.Fatal(err)
			}
		}
	}
}
