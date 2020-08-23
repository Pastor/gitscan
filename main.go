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
	log.Println("Fetch ", dir)
	cmd := exec.Command("git", "fetch", "--all", "--tags")
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	_ = cmd.Run()
	log.Println("Pull ", dir)
	cmd = exec.Command("git", "pull", "--all")
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	_ = cmd.Run()
	if _, err := os.Stat(path.Join(dir, ".gitmodules")); !os.IsNotExist(err) {
		log.Println("Submodule ", dir)
		cmd := exec.Command("git", "submodule", "update", "--remote")
		cmd.Dir = dir
		cmd.Stdout = os.Stdout
		_ = cmd.Run()
	}
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
