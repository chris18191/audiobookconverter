package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/viper"
)

type ProcessingState int

const (
	Error ProcessingState = iota
	New
	InProcess
	Done
)

func (ps ProcessingState) String() string {
	return map[ProcessingState]string{
		Error:     "Unknown",
		New:       "Not yet processed",
		InProcess: "Currently processing",
		Done:      "Finished",
	}[ps]
}

type Book struct {
	EpubPath      string
	BookName      string
	SourceFolder  string
	TargetFolder  string
	Status        ProcessingState
	TimeRemaining time.Duration
	Progress      int
}

const (
	ENV_PREFIX = "CONVERTER"
)

var (
	VOICE      = "bm_lewis"
	FOLDER_IN  = ""
	FOLDER_OUT = ""

	TRAVERSAL_MAX_DEPTH = 3
	FFMPEG_AVAILABLE    = false

	books = make(map[string]Book)
)

func main() {

	// CHECKS
	performChecks()
	// CHECKS OK!

	for {
		// Read input folder
		files, err := traverseAndFindEpub(FOLDER_IN)
		if err != nil {
			fmt.Printf("Failed to traverse input folder '%s': %s\n", FOLDER_IN, err)
		}
		fmt.Println("Found following epubs as input: ", files)
		parseAndAddBooks(&books, files, true)

		// Read output folder and parse books
		// Every output folder has to contain the original epub file in order to be recognized
		files, err = traverseAndFindEpub(FOLDER_OUT)
		if err != nil {
			fmt.Printf("Failed to traverse output folder '%s': %s\n", FOLDER_OUT, err)
		}
		fmt.Println("Found following epubs as output: ", files)
		parseAndAddBooks(&books, files, false)

		printBooks(&books)

		processBooks(&books)

		log.Println("Waiting for new files...")
		time.Sleep(time.Hour * 1)
	}
}

func processBooks(books *map[string]Book) {
	for epub, book := range *books {
		if book.Status == New {
			// Process new book and wait for it to finish
			_tmp := strings.Split(epub, ".")
			bookName := strings.Join(_tmp[:len(_tmp)-1], ".")
			bookFolder := filepath.Join(FOLDER_OUT, bookName)

			log.Printf("Processing book '%s' ...\n", bookName)
			result_folder, err := runAudiblezOnEpub(book)
			if err != nil {
				fmt.Printf("failed to process book: %s\n", err)
				continue
			}

			cmdFFMPEG := exec.Command("bash", "-c", "for f in ./*.wav; do ffmpeg -i $f -ac 2 -b:a 192k  ${f#*_xhtml_}.mp3 ; done")
			cmdFFMPEG.Dir = result_folder
			output, err := cmdFFMPEG.CombinedOutput()
			if err != nil {
				fmt.Println("Output:\n", string(output))
				fmt.Println("Error while trying to convert audio files: ", err)
			}

			moveFiles(result_folder, bookFolder, []string{".wav", ".txt"})

			os.RemoveAll(result_folder)
		}
	}
}

func printBooks(books *map[string]Book) {
	for epub, book := range *books {
		fmt.Println("File: ", epub, "\t Status: ", book.Status)
	}
}

func parseAndAddBooks(booksMap *map[string]Book, epubs []string, areBooksInImportFolder bool) {
	for _, epub := range epubs {
		// Get basename of the file
		basename := filepath.Base(epub)
		// Check whether book already exists
		var bookEntry Book
		if _, ok := (*booksMap)[basename]; ok {
			bookEntry = (*booksMap)[basename]
		} else {
			bookEntry.BookName = basename
			bookEntry.EpubPath = epub
		}

		if areBooksInImportFolder {
			bookEntry.SourceFolder = filepath.Dir(epub)
		} else {
			bookEntry.TargetFolder = filepath.Dir(epub)
		}

		bookEntry.Status = Error

		if bookEntry.SourceFolder != "" {
			bookEntry.Status = New
		}
		if bookEntry.TargetFolder != "" {
			bookEntry.Status = Done
		}

		(*booksMap)[basename] = bookEntry
	}
}

func performChecks() {
	// Configure Viper for environment variables
	viper.AutomaticEnv()
	viper.SetEnvPrefix(ENV_PREFIX)
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))

	// Retrieve folder paths from environment variables
	FOLDER_IN = viper.GetString("in")
	FOLDER_OUT = viper.GetString("out")
	if viper.IsSet("max_depth") {
		TRAVERSAL_MAX_DEPTH = viper.GetInt("max_depth")
	}

	// Log folder paths
	log.Printf("%s_IN: %s\n", ENV_PREFIX, FOLDER_IN)
	log.Printf("%s_OUT: %s\n", ENV_PREFIX, FOLDER_OUT)
	log.Printf("%s_MAX_DEPTH: %d\n", ENV_PREFIX, TRAVERSAL_MAX_DEPTH)

	// Validate folder paths
	if FOLDER_IN == "" || FOLDER_OUT == "" {
		log.Println("Error: At least one folder is not specified!")
		os.Exit(1)
	}

	// Check if audiblez binary is available
	if _, err := exec.LookPath("audiblez"); err != nil {
		log.Fatal("Error: Could not find 'audiblez' executable: ", err)
	}

	// Check if ffmpeg binary is available
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		log.Fatal("Note: Could not find 'ffmpeg' executable. Not going to ")
	}

	log.Println("All checks passed. Starting...")
}

func runAudiblezOnEpub(book Book) (string, error) {

	log.Println("Running for book: ", book)

	tmpPath := "/tmp/audiobookconverter/" + time.Now().Format("2006-01-02_15_04_05")

	source, err := os.Open(book.EpubPath)
	if err != nil {
		return "", fmt.Errorf("Could not read epub file: %s", err)
	}
	defer source.Close()

	err = os.MkdirAll(tmpPath, os.ModePerm)
	if err != nil {
		return "", fmt.Errorf("Failed to create temp folder: %s", err)
	}
	destination, err := os.Create(tmpPath + "/book.epub")
	if err != nil {
		return "", fmt.Errorf("Could not create temp file: %s", err)
	}
	defer destination.Close()
	_, err = io.Copy(destination, source)
	if err != nil {
		return "", fmt.Errorf("Failed to copy ebook to temp folder: %s", err)
	}

	cmd := exec.Command("audiblez", "-v", VOICE, "book.epub")
	// stdor, stdow := io.Pipe()
	// stder, stdew := io.Pipe()
	// cmd.Stdout = stdow
	// cmd.Stderr = stdew

	// stdor, _ := cmd.StdoutPipe()
	stder, _ := cmd.StderrPipe()

	cmd.Dir = tmpPath

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("Failed to run command: %s", err)
	}

	// var wg sync.WaitGroup
	// wg.Add(1)

	// Goroutine to read and evaluate the command's output
	// go func() {
	// 	log.Println("Starting output reading...")
	// 	defer wg.Done()
	// 	reader := bufio.NewReader(stdor)

	// 	var timeRemaining = ""
	// 	var progress = ""
	// 	for {
	// 		log.Println("Scanned line...")
	// 		_line, _, _ := reader.ReadLine()
	// 		line := string(_line)
	// 		log.Println("OUT: ", line)
	// 		if err != nil {
	// 			if err == io.EOF {
	// 				break
	// 			}
	// 			log.Println("Failed to read line: ", err)
	// 			continue
	// 		}
	// 		log.Println(line)
	// 		if strings.HasPrefix(line, "Estimated time remaining") {
	// 			timeRemaining = line[24:]
	// 		} else if strings.HasPrefix(line, "Progress") {
	// 			progress = line[10:]
	// 		}
	// 		log.Println("Current progress:", progress)
	// 		log.Println("Remaining time:", timeRemaining)
	// 	}

	// 	fmt.Println("Finished output reading.")
	// }()

	// go func() {
	// 	reader := bufio.NewReader(stder)
	// 	for {
	// 		line, _, _ := reader.ReadLine()
	// 		log.Println("ERROR: ", string(line))
	// 	}
	// }()

	// wg.Wait()

	tmp, _ := io.ReadAll(stder)
	// Wait for the command to finish
	if err := cmd.Wait(); err != nil {
		log.Printf("Command finished with error: %v\n#### OUTPUT ####\n%s", err, tmp)
		log.Printf("Epub will be copied to prevent future retries.")
	}

	os.Rename(filepath.Join(tmpPath, "book.epub"), filepath.Join(tmpPath, book.BookName))

	log.Printf("Finished book '%s' in '%s'\n", book.BookName, tmpPath)
	return tmpPath, nil
}

// moveFiles moves all files from the source directory to the destination directory.
// Parameters:
//   - srcDir: The source directory path.
//   - destDir: The destination directory path.
//
// Returns:
//   - An error if the operation fails.
func moveFiles(srcDir string, destDir string, excludeExtensions []string) error {
	// Open the source directory
	files, err := os.ReadDir(srcDir)

	if err != nil {
		return fmt.Errorf("failed to read source directory: %v", err)
	}

	// Ensure the destination directory exists
	err = os.MkdirAll(destDir, os.ModePerm)
	if err != nil {
		return fmt.Errorf("failed to create destination directory: %v", err)
	}

	// Iterate over each file in the source directory
	for _, file := range files {
		srcPath := filepath.Join(srcDir, file.Name())
		if slices.Contains(excludeExtensions, filepath.Ext(srcPath)) {
			log.Println("Skipping file copy for: ", srcPath)
			continue
		}
		destPath := filepath.Join(destDir, file.Name())

		// log.Printf("Moving '%s' to '%s'", srcPath, destPath)

		// Move the file
		destination, err := os.Create(destPath)
		if err != nil {
			return fmt.Errorf("Could not create destination file: %s", err)
		}
		source, err := os.Open(srcPath)
		if err != nil {
			return fmt.Errorf("Could not open source file: %s", err)
		}
		defer destination.Close()
		_, err = io.Copy(destination, source)
		// if err != nil {
		// 	return fmt.Errorf("failed to move file '%s' to '%s': %v", srcPath, destPath, err)
		// }
	}

	return nil
}

// traverseAndFindEpub traverses a given path to a specified depth and searches for files with an .epub extension.
// Parameters:
//   - path: The starting directory path.
//   - TRAVERSAL_MAX_DEPTH: The maximum depth to traverse. Use -1 for unlimited depth.
//
// Returns:
//   - A slice of strings containing the paths of all found .epub files.
//   - An error if the traversal fails.
func traverseAndFindEpub(path string) ([]string, error) {
	var epubFiles []string

	// Helper function to perform the traversal
	traverse := func(currentPath string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Calculate current depth
		currentDepth := strings.Count(currentPath, string(os.PathSeparator))

		// Skip if the current depth exceeds TRAVERSAL_MAX_DEPTH (unless TRAVERSAL_MAX_DEPTH is -1)
		if TRAVERSAL_MAX_DEPTH != -1 && currentDepth > TRAVERSAL_MAX_DEPTH {
			return filepath.SkipDir
		}

		// Check if the current file is an .epub file
		if !info.IsDir() && strings.HasSuffix(strings.ToLower(info.Name()), ".epub") {
			epubFiles = append(epubFiles, currentPath)
		}

		return nil
	}

	// Start traversal
	err := filepath.Walk(path, traverse)

	if err != nil {
		return nil, fmt.Errorf("failed to traverse directory: %v", err)
	}

	return epubFiles, nil
}
