// Package githttp implements a http protocol backend for git.
package githttp

import (
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type service struct {
	method  string
	handler func(handlerReq)
	rpc     string
	pattern string
	regx    *regexp.Regexp
}

var gitBinPath = "/usr/bin/git"

type handlerReq struct {
	w           http.ResponseWriter
	r           *http.Request
	Rpc         string
	Dir         string
	File        string
	writeAccess bool
}

var services = []*service{
	newService("POST", "(.*?)/git-upload-pack$", "upload-pack", serviceRpc),
	newService("POST", "(.*?)/git-receive-pack$", "receive-pack", serviceRpc),
	newService("GET", "(.*?)/info/refs$", "", getInfoRefs),
	newService("GET", "(.*?)/HEAD$", "", getTextFile),
	newService("GET", "(.*?)/objects/info/alternates$", "", getTextFile),
	newService("GET", "(.*?)/objects/info/http-alternates$", "", getTextFile),
	newService("GET", "(.*?)/objects/info/packs$", "", getInfoPacks),
	newService("GET", "(.*?)/objects/info/[^/]*$", "", getTextFile),
	newService("GET", "(.*?)/objects/[0-9a-f]{2}/[0-9a-f]{38}$", "", getLooseObject),
	newService("GET", "(.*?)/objects/pack/pack-[0-9a-f]{40}\\.pack$", "", getPackFile),
	newService("GET", "(.*?)/objects/pack/pack-[0-9a-f]{40}\\.idx$", "", getIdxFile),
}

func newService(method, pattern, rpc string, handler func(handlerReq)) *service {
	re, err := regexp.Compile(pattern)
	if err != nil {
		panic(err)
	}

	return &service{
		method:  method,
		pattern: pattern,
		rpc:     rpc,
		handler: handler,
		regx:    re,
	}
}

func Handle(w http.ResponseWriter, r *http.Request, repoDir string, writeAccess bool) {
	for _, service := range services {
		if m := service.regx.FindStringSubmatch(r.URL.Path); m != nil {
			if service.method != r.Method {
				renderMethodNotAllowed(w, r)
				return
			}

			if _, err := os.Stat(repoDir); os.IsNotExist(err) {
				log.Print(err)
				renderNotFound(w)
				return
			}

			rpc := service.rpc
			file := strings.Replace(r.URL.Path, m[1]+"/", "", 1)
			hr := handlerReq{w, r, rpc, repoDir, file, writeAccess}
			service.handler(hr)
			return
		}
	}
	renderNotFound(w)
	return
}

func serviceRpc(hr handlerReq) {
	w, r, rpc, dir := hr.w, hr.r, hr.Rpc, hr.Dir
	access := hasAccess(r, dir, rpc, hr.writeAccess, true)

	if !access {
		renderNoAccess(w)
		return
	}

	input, _ := ioutil.ReadAll(r.Body)

	w.Header().Set("Content-Type", fmt.Sprintf("application/x-git-%s-result", rpc))
	w.WriteHeader(http.StatusOK)

	args := []string{rpc, "--stateless-rpc", dir}
	cmd := exec.Command(gitBinPath, args...)
	cmd.Dir = dir
	in, err := cmd.StdinPipe()
	if err != nil {
		log.Print(err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Print(err)
	}

	err = cmd.Start()
	if err != nil {
		log.Print(err)
	}

	in.Write(input)
	io.Copy(w, stdout)
	cmd.Wait()
}

func getInfoRefs(hr handlerReq) {
	w, r, dir := hr.w, hr.r, hr.Dir
	service_name := getServiceType(r)

	access := hasAccess(r, dir, service_name, hr.writeAccess, false)
	if !access {
		renderNoAccess(w)
		return
	}

	args := []string{service_name, "--stateless-rpc", "--advertise-refs", "."}
	refs := gitCommand(dir, args...)

	hdrNocache(w)
	w.Header().Set("Content-Type", fmt.Sprintf("application/x-git-%s-advertisement", service_name))
	w.WriteHeader(http.StatusOK)
	w.Write(packetWrite("# service=git-" + service_name + "\n"))
	w.Write(packetFlush())
	w.Write(refs)
}

func getInfoPacks(hr handlerReq) {
	hdrCacheForever(hr.w)
	sendFile("text/plain; charset=utf-8", hr)
}

func getLooseObject(hr handlerReq) {
	hdrCacheForever(hr.w)
	sendFile("application/x-git-loose-object", hr)
}

func getPackFile(hr handlerReq) {
	hdrCacheForever(hr.w)
	sendFile("application/x-git-packed-objects", hr)
}

func getIdxFile(hr handlerReq) {
	hdrCacheForever(hr.w)
	sendFile("application/x-git-packed-objects-toc", hr)
}

func getTextFile(hr handlerReq) {
	hdrNocache(hr.w)
	sendFile("text/plain", hr)
}

func sendFile(content_type string, hr handlerReq) {
	w, r := hr.w, hr.r
	req_file := path.Join(hr.Dir, hr.File)

	f, err := os.Stat(req_file)
	if os.IsNotExist(err) {
		renderNotFound(w)
		return
	}

	w.Header().Set("Content-Type", content_type)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", f.Size()))
	w.Header().Set("Last-Modified", f.ModTime().Format(http.TimeFormat))
	http.ServeFile(w, r, req_file)
}

func getServiceType(r *http.Request) string {
	service_type := r.FormValue("service")

	if s := strings.HasPrefix(service_type, "git-"); !s {
		return ""
	}

	return strings.Replace(service_type, "git-", "", 1)
}

func hasAccess(r *http.Request, dir string, rpc string, writeAccess, check_content_type bool) bool {
	if check_content_type {
		if r.Header.Get("Content-Type") != fmt.Sprintf("application/x-git-%s-request", rpc) {
			return false
		}
	}

	if rpc != "upload-pack" && rpc != "receive-pack" {
		return false
	}

	if rpc == "receive-pack" {
		return writeAccess
	}

	if rpc == "upload-pack" {
		return true
	}

	return getConfigSetting(rpc, dir)
}

func getConfigSetting(service_name string, dir string) bool {
	service_name = strings.Replace(service_name, "-", "", -1)
	setting := getGitConfig("http."+service_name, dir)

	if service_name == "uploadpack" {
		return setting != "false"
	}

	return setting == "true"
}

func getGitConfig(config_name string, dir string) string {
	args := []string{"config", config_name}
	out := string(gitCommand(dir, args...))
	return out[0 : len(out)-1]
}

func updateServerInfo(dir string) []byte {
	args := []string{"update-server-info"}
	return gitCommand(dir, args...)
}

func gitCommand(dir string, args ...string) []byte {
	command := exec.Command(gitBinPath, args...)
	command.Dir = dir
	out, err := command.Output()

	if err != nil {
		log.Print(err)
	}

	return out
}

func renderMethodNotAllowed(w http.ResponseWriter, r *http.Request) {
	if r.Proto == "HTTP/1.1" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		w.Write([]byte("Method Not Allowed"))
	} else {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("Bad Request"))
	}
}

func renderNotFound(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNotFound)
	w.Write([]byte("Not Found"))
}

func renderNoAccess(w http.ResponseWriter) {
	w.WriteHeader(http.StatusForbidden)
	w.Write([]byte("Forbidden"))
}

func packetFlush() []byte {
	return []byte("0000")
}

func packetWrite(str string) []byte {
	s := strconv.FormatInt(int64(len(str)+4), 16)

	if len(s)%4 != 0 {
		s = strings.Repeat("0", 4-len(s)%4) + s
	}

	return []byte(s + str)
}

func hdrNocache(w http.ResponseWriter) {
	w.Header().Set("Expires", "Fri, 01 Jan 1980 00:00:00 GMT")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Cache-Control", "no-cache, max-age=0, must-revalidate")
}

func hdrCacheForever(w http.ResponseWriter) {
	now := time.Now().Unix()
	expires := now + 31536000
	w.Header().Set("Date", fmt.Sprintf("%d", now))
	w.Header().Set("Expires", fmt.Sprintf("%d", expires))
	w.Header().Set("Cache-Control", "public, max-age=31536000")
}
