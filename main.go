package main

import (
	"flag"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path"
)

type DoDirectory func(string) bool

func pullGit(dir string) bool {
	log.Println("Pull ", dir)
	cmd := exec.Command("git", "pull")
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Run()
	return true
}

func findGit(dir string, cb DoDirectory) {
	files, err := ioutil.ReadDir(dir)
	if err != nil {
		log.Fatal(err)
	}

	for _, f := range files {
		absFileName := path.Join(dir, f.Name())
		if f.IsDir() {
			if f.Name() == ".git" {
				cb(dir)
				return
			} else {
				findGit(absFileName, cb)
			}
		}
	}
}

func main() {
	var directory string
	cwd, _ := os.Getwd()
	flag.StringVar(&directory, "scan_dir", cwd, "")
	flag.Parse()
	findGit(directory, pullGit)
}
