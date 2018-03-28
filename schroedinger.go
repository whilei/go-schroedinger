package schroedinger

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gopkg.in/yaml.v2"
)

const commentPattern = "#"

var errCommentLine = errors.New("comment line")
var errEmptyLine = errors.New("empty line")

// different for windows
var goExecutablePath string
var commandPrefix []string

type Config struct {
	GlobalTrialsAllowed int `yaml:"defaultTrialsAllowed"`
	Tests Tests
}
var config *Config

type Tests []*Test
type Test struct {
	Name          string
	AnyFailing bool `yaml:"anyFailing"`
	TrialsDone    int `yaml:-`
	TrialsAllowed int `yaml:"trialsAllowed""`
	Cases []*Case
}

type Case struct {
	Name string
	TrialsDone    int `yaml:-`
	TrialsAllowed int `yaml:"trialsAllowed""`
}

func init() {
	goExecutablePath = getGoPath()
	commandPrefix = getCommandPrefix()
}

func (t *Test) getCase(name string) *Case {
	for _, c := range t.Cases {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func (t *Test) getTrialsAllowed() int {
	if t.TrialsAllowed != 0 {
		return t.TrialsAllowed
	}
	return config.GlobalTrialsAllowed
}

func (t *Test) buildTestFromCase(caseName string) (*Test, error) {
	c := t.getCase(caseName)
	if c == nil {
		return nil, fmt.Errorf("no test case found: %s %s", t.Name, caseName)
	}
	out := &Test{
		Name: fmt.Sprintf("%s -run %s", getNonRecursivePackageName(t.Name), c.Name),
		TrialsDone: t.TrialsDone,
	}
	if c.TrialsAllowed != 0 {
		out.TrialsAllowed = c.TrialsAllowed
	}
	return out, nil
}

func getNonRecursivePackageName(s string) string {
	out := strings.TrimSuffix(s, string(filepath.Separator)+"...")
	out = strings.TrimSuffix(out, "...")
	return out
}

func (t *Test) String() string {
	return fmt.Sprintf("%s (anyFailing=%v, trialsAllowed=%v)", t.Name, t.AnyFailing, t.TrialsAllowed)
}

func getGoPath() string {
	return filepath.Join(runtime.GOROOT(), "bin", "go")
}

func getCommandPrefix() []string {
	if runtime.GOOS == "windows" {
		return []string{"cmd", "/C"}
	}
	return []string{"/bin/sh", "-c"}
}

func parseMatchList(list string) []string {
	// eg. "", "downloader,fetcher", "sync"
	if len(list) == 0 {
		return nil
	}
	ll := strings.Trim(list, " ")
	return strings.Split(ll, ",")
}

func testMatchesList(t Test, whites, blacks []string) bool {
	if blacks != nil && len(blacks) > 0 {
		for _, m := range blacks {
			if strings.Contains(t.Name, m) {
				return false
			}
			for _, c := range t.Cases {
				if strings.Contains(c.Name, m) {
					return false
				}
			}
		}
	}
	if whites != nil && len(whites) > 0 {
		for _, m := range whites {
			if !strings.Contains(t.Name, m) {
				return false
			} else {
				return true
			}
			for _, c := range t.Cases {
				if !strings.Contains(c.Name, m) {
					return false
				} else {
					return true
				}
			}
		}
	}
	return true
}

func setConfigFromFile(f string, allowed func (*Test) bool) (err error) {
	file, err := os.Open(f)
	if err != nil {
		return
	}
	defer file.Close()

	var b []byte
	_, err = file.Read(b)
	if err != nil {
		return
	}


	err = yaml.Unmarshal(b, &config)
	if err != nil {
		return
	}

	return err
}

func filterTests(tests Tests, allowed func(*Test) bool) Tests {
	var out Tests
	for _, t := range tests {
		if allowed(t) {
			out = append(out, t)
		}
	}
	return out
}

func grepFailures(gotestout []byte) []string {
	reader := bytes.NewReader(gotestout)
	scanner := bufio.NewScanner(reader)

	var fails []string

	for scanner.Scan() {
		// eg. '--- FAIL: TestFastCriticalRestarts64 (12.34s)'
		text := scanner.Text()
		if !strings.Contains(text, "--- FAIL:") {
			continue
		}
		step1 := strings.Split(text, ":")
		step2 := strings.Split(step1[1], "(")
		testname := strings.Trim(step2[0], " ")
		fails = append(fails, testname)
	}

	if e := scanner.Err(); e != nil {
		log.Fatal(e)
	}

	return fails
}

func runTest(t *Test) ([]byte, error) {
	args := fmt.Sprintf("test %s", t.Name) // eg. 'go test ____'
	log.Println("|", commandPrefix[0], commandPrefix[1], goExecutablePath+" "+args)
	cmd := exec.Command(commandPrefix[0], commandPrefix[1], goExecutablePath+" "+args)
	out, err := cmd.CombinedOutput()
	t.TrialsDone++
	return out, err
}

func tryTestCase(t *Test, c chan error) {
	for t.TrialsDone < t.getTrialsAllowed() {
		start := time.Now()
		if o, e := runTest(t); e == nil {
			log.Println(t)
			log.Printf("- PASS (%v) %d/%d", time.Since(start), t.TrialsDone, t.getTrialsAllowed())
			c <- nil
			return
		} else {
			log.Println(t)
			log.Printf("- FAIL (%v) %d/%d: %v", time.Since(start), t.TrialsDone, t.getTrialsAllowed(), e)
			fmt.Println()
			fmt.Println(string(o))
		}
	}
	c <- fmt.Errorf("FAIL %s", t.Name)
}

// only gets to send one nil/error on the given channel
func tryPackageTest(t *Test, c chan error) {
	start := time.Now()
	if o, e := runTest(t); e == nil {
		log.Println(t)
		log.Printf("- PASS (%v)", time.Since(start))
		fmt.Println()
		fmt.Println(string(o))
		c <- nil
		return
	} else {
		log.Println(t)
		log.Printf("- FAIL (%v)", time.Since(start))
		fmt.Println()
		fmt.Println(string(o))

		fails := grepFailures(o)
		if len(fails) == 0 {
			log.Fatalf("%s reported failure, but no failing tests were discovered, err=%v",
				getNonRecursivePackageName(t.Name), e)
		}

		var failingTests []*Test
		for _, f := range fails {
			failingTests = append(failingTests,
				&Test{
					Pkg:        pkg(getNonRecursivePackageName(t.Pkg)),
					Name:       f,
					TrialsDone: 1,
				})
		}
		log.Printf("Found failing Test(s) in %s: %v. Rerunning...",
			getNonRecursivePackageName(t.Pkg),
			fails,
		)

		pc := make(chan error, len(failingTests))
		for _, f := range failingTests {
			go tryTestCase(f, pc)
		}
		for i := 0; i < len(failingTests); i++ {
			if e := <-pc; e != nil {
				c <- e
				return
			}
		}
		c <- nil
	}
}

func tryTest(t *Test, c chan error) {
	if len(t.Cases) != 0 {
		tryTestCase(t, c)
	} else {
		tryPackageTest(t, c)
	}
}


func Run(testsFile, whitelistMatch, blacklistMatch string, trialsN int) {
	e := run(testsFile, whitelistMatch, blacklistMatch, trialsN)
	if e != nil {
		log.Fatal(e)
	}
}

func run(testsFile, whitelistMatch, blacklistMatch string, trialsN int) error {
	if trialsN == 0 {
		return fmt.Errorf("trials allowed must be >0, got: %d", trialsN)
	}
	config.GlobalTrialsAllowed = trialsN

	whites := parseMatchList(whitelistMatch)
	blacks := parseMatchList(blacklistMatch)

	testsFile = filepath.Clean(testsFile)
	testsFile, _ = filepath.Abs(testsFile)

	allowed := func(t *Test) bool {
		return testMatchesList(*t, whites, blacks)
	}

	err := setConfigFromFile(testsFile, allowed)
	if err != nil {
		return err
	}

	log.Println("* go executable path:", goExecutablePath)
	log.Println("* command prefix:", strings.Join(commandPrefix, " "))
	log.Println("* tests file:", testsFile)
	log.Println("* TrialsDone allowed: ", globalTrialsAllowed)
	log.Println("* blacklist: ", blacks)
	log.Println("* whitelist: ", whites)
	log.Printf("* running %d/%d tests", len(tests), len(alltests))

	var results = make(chan error, len(tests))

	allstart := time.Now()
	defer func() {
		log.Printf("FINISHED (%v)", time.Since(allstart))
	}()

	for _, t := range tests {
		go tryTest(t, results)
	}

	for i := 0; i < len(tests); i++ {
		if e := <-results; e != nil {
			return e
		}
	}

	close(results)
	return nil
}
