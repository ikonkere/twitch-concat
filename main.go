package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kr/logfmt"
	"github.com/schollz/progressbar/v3"
)

//new style of edgecast links: https://vod089-ttvnw.akamaized.net/1059582120fbff1a392a_reinierboortman_26420932624_719978480/chunked/highlight-180380104.m3u8
//old style of edgecast links: https://vod164-ttvnw.akamaized.net/7a16586e4b7ef40300ba_zizaran_27258736688_772341213/chunked/index-dvr.m3u8

const edgecastLinkBegin string = "https://"
const edgecastLinkBaseEndOld string = "index"
const edgecastLinkBaseEnd string = "highlight"
const edgecastLinkM3U8End string = ".m3u8"
const targetdurationStart string = "TARGETDURATION:"
const targetdurationEnd string = "\n#ID3"
const resolutionStart string = `NAME="`
const resolutionEnd string = `"`
const qualityStart string = `VIDEO="`
const qualityEnd string = `"`
const sourceQuality string = "chunked"
const chunkFileExtension string = ".ts"

var ffmpegCMD = `ffmpeg`

var debug bool

//var twitchClientID = "kimne78kx3ncx6brgo4mv6wki5h1ko"
var twitchClientID = "b31o4btkqth5bzbvr9ub2ovr79umhh"

var concurrentDownloads int

var maxTryCount *int
var audio *bool
var audioOnly *bool

var m3u8 *string

/*
	Returns the signature and token from a tokenAPILink
	signature and token are needed for accessing the usher api
*/
func accessTokenAPI(tokenAPILink string) (string, string, error) {
	printDebugf("\ntokenAPILink: %s\n", tokenAPILink)

	resp, err := http.Get(tokenAPILink)
	if err != nil {
		return "", "", err
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", "", err
	}

	// See https://blog.golang.org/json-and-go "Decoding arbitrary data"
	var data interface{}
	err = json.Unmarshal(body, &data)
	m := data.(map[string]interface{})
	sig := fmt.Sprintf("%v", m["sig"])
	token := fmt.Sprintf("%v", m["token"])
	return sig, token, err
}

func accessUsherAPI(usherAPILink string) (map[string]string, error) {
	resp, err := http.Get(usherAPILink)
	if err != nil {
		return make(map[string]string), err
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return make(map[string]string), err
	}

	respString := string(body)

	printDebugf("\nUsher API response:\n%s\n", respString)

	var re = regexp.MustCompile(qualityStart + "([^\"]+)" + qualityEnd + "\n([^\n]+)\n")
	match := re.FindAllStringSubmatch(respString, -1)

	edgecastURLmap := make(map[string]string)

	for _, element := range match {
		edgecastURLmap[element[1]] = element[2]
	}

	return edgecastURLmap, err
}

func getM3U8List(m3u8Link string) (string, error) {
	resp, err := http.Get(m3u8Link)
	if err != nil {
		return "", err
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	return string(body), err
}

/*
	Returns the number of chunks to download based of the start and end time and the target duration of a
	chunk. Adding 1 to overshoot the end by a bit
*/
func calcChunkCount(sh int, sm int, ss int, eh int, em int, es int, target int) int {
	startSeconds := toSeconds(sh, sm, ss)
	endSeconds := toSeconds(eh, em, es)

	return ((endSeconds - startSeconds) / target) + 1
}

func startingChunk(sh int, sm int, ss int, target int) int {
	startSeconds := toSeconds(sh, sm, ss)
	return (startSeconds / target)
}

func toSeconds(sh int, sm int, ss int) int {
	return sh*3600 + sm*60 + ss
}

func downloadChunk(newpath string, edgecastBaseURL string, chunkCount string, chunkName string, vodID string) int64 {
	var chunkLength int64

	chunkURL := edgecastBaseURL + chunkName

	downloadPath := newpath + "/" + vodID + "_" + chunkCount + chunkFileExtension

	if fi, err := os.Stat(downloadPath); !os.IsNotExist(err) {
		if debug {
			fmt.Printf("Chunk exists: %s\n", chunkURL)
		}
		return fi.Size()
	}

	if debug {
		fmt.Printf("Downloading: %s\n", chunkURL)
	}

	httpClient := http.Client{
		Timeout: 30 * time.Second,
	}

	var body []byte

	for retryCount := 0; retryCount < *maxTryCount || *maxTryCount == 0; retryCount++ {
		if retryCount > 0 {
			printDebugf("%d. retry: chunk '%s'\n", retryCount, chunkName)
		}

		body = nil

		resp, err := httpClient.Get(chunkURL)

		if err != nil {
			printFatal(err, "Could not download chunk", chunkName)
			return -1
		}

		if resp.StatusCode != 200 {
			body, _ := ioutil.ReadAll(resp.Body)
			resp.Body.Close()
			printDebugf("StatusCode: %d; %s; Could not download chunk '%s'", resp.StatusCode, string(body), chunkURL)
			return -1
		}

		body, err = ioutil.ReadAll(resp.Body)
		resp.Body.Close()

		if err != nil {

			if retryCount == *maxTryCount-1 {
				printFatal(err, "Could not download chunk", chunkURL, "after", *maxTryCount, "tries.")
				return -1
			} else {
				printDebug("Could not download chunk", chunkURL)
				printDebug(err)
			}

		} else {
			break
		}
	}

	chunkLength = int64(len(body))
	_ = ioutil.WriteFile(downloadPath, body, 0644)

	return chunkLength
}

func createConcatFile(newpath string, chunkNum int, startChunk int, vodID string) (*os.File, error) {
	tempFile, err := ioutil.TempFile(newpath, "twitchVod_"+vodID+"_")
	if err != nil {
		return nil, err
	}
	defer tempFile.Close()
	concat := ``
	for i := startChunk; i < (startChunk + chunkNum); i++ {
		s := strconv.Itoa(i)
		filePath, _ := filepath.Abs(newpath + "/" + vodID + "_" + s + chunkFileExtension)
		concat += "file '" + filePath + "'\n"
	}

	if _, err := tempFile.WriteString(concat); err != nil {
		return nil, err
	}
	return tempFile, nil
}

func ffmpegCombine(newpath string, chunkNum int, startChunk int, vodID string, vodSavePath string) {
	tempFile, err := createConcatFile(newpath, chunkNum, startChunk, vodID)
	if err != nil {
		fmt.Println(err)
		return
	}
	defer os.Remove(tempFile.Name())

	args := []string{"-progress", "tcp://localhost:9998", "-f", "concat", "-safe", "0", "-i", tempFile.Name(), "-c", "copy", "-bsf:a", "aac_adtstoasc", "-fflags", "+genpts", vodSavePath}

	if debug {
		fmt.Printf("Running ffmpeg: %s %s\n", ffmpegCMD, args)
	}

	cmd := exec.Command(ffmpegCMD, args...)
	var errbuf bytes.Buffer
	cmd.Stderr = &errbuf
	err = cmd.Run()
	if err != nil {
		fmt.Println(errbuf.String())
		fmt.Println("ffmpeg error")
	}

	if *audio || *audioOnly {
		if debug {
			fmt.Print("Running ffmpeg audio extraction")
		}
		fmt.Println("Extracting audio...")

		audioSavePath := vodSavePath[:len(vodSavePath)-3] + "mp3"
		args := []string{"-i", vodSavePath, "-f", "mp3", "-vn", audioSavePath}

		cmd := exec.Command(ffmpegCMD, args...)
		var errbuf bytes.Buffer
		cmd.Stderr = &errbuf
		err = cmd.Run()
		if err != nil {
			fmt.Println(errbuf.String())
			fmt.Println("ffmpeg error")
		}

		if *audioOnly {
			os.Remove(vodSavePath)
		}
	}
}

type FfmpegStatusData struct {
	Data map[string][]byte
}

func (mm FfmpegStatusData) HandleLogfmt(key, val []byte) error {
	mm.Data[string(key)] = val
	return nil
}

func ffmpegStatus(duration int) {
	l, err := net.Listen("tcp", ":9998")
	if err != nil {
		printFatal(err)
		return
	}

	defer l.Close()

	c, err := l.Accept()
	if err != nil {
		printFatal(err)
		return
	}

	defer c.Close()

	bar := progressbar.NewOptions(duration,
		progressbar.OptionSetRenderBlankState(!debug),
	)

	var status FfmpegStatusData

	for {
		data := make([]byte, 1024)

		_, err := c.Read(data)
		if err != nil {
			if err != io.EOF {
				printFatal(err)
			} else {
				ffmpegLogProgress(&status, data, bar)
			}
			break
		}

		status = *ffmpegLogProgress(&status, data, bar)
	}

	if !debug {
		bar.Finish()
	}
}

func ffmpegLogProgress(prev *FfmpegStatusData, data []byte, bar *progressbar.ProgressBar) *FfmpegStatusData {
	curr := &FfmpegStatusData{
		Data: make(map[string][]byte),
	}
	if err := logfmt.Unmarshal(data, curr); err != nil {
		printFatal(err)
		return nil
	}

	v1, _ := strconv.Atoi(string(curr.Data["out_time_us"]))
	v2, _ := strconv.Atoi(string(prev.Data["out_time_us"]))
	if !debug {
		bar.Add((v1 - v2) / 1000000)
	} else {
		for k, v := range curr.Data {
			fmt.Printf("%s=%s\n", k, string(v))
		}
	}

	return curr
}

func deleteChunks(newpath string, chunkCount int, startChunk int, vodID string) {
	var del string
	for i := startChunk; i < (startChunk + chunkCount); i++ {
		s := strconv.Itoa(i)
		del = newpath + "/" + vodID + "_" + s + chunkFileExtension
		err := os.Remove(del)
		if err != nil {
			fmt.Println("Could not delete all chunks, try manually deleting them", err)
		}
	}
}

func printQualityOptions(vodIDString string) {
	vodID, _ := strconv.Atoi(vodIDString)

	tokenAPILink := fmt.Sprintf("https://api.twitch.tv/api/vods/%v/access_token?&client_id="+twitchClientID, vodID)

	fmt.Println("Contacting Twitch Server")

	sig, token, err := accessTokenAPI(tokenAPILink)
	if err != nil {
		printFatal(err, "Could not access twitch token api")
	}

	usherAPILink := fmt.Sprintf("http://usher.twitch.tv/vod/%v?nauthsig=%v&nauth=%v&allow_source=true", vodID, sig, token)

	resp, err := http.Get(usherAPILink)
	if err != nil {
		printFatal(err, "Could not download qualitiy options")
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		printFatal(err, "Could not read qualitiy options")
	}

	respString := string(body)

	qualityCount := strings.Count(respString, resolutionStart)
	for i := 0; i < qualityCount; i++ {
		rs := strings.Index(respString, resolutionStart) + len(resolutionStart)
		re := strings.Index(respString[rs:], resolutionEnd) + rs
		qs := strings.Index(respString, qualityStart) + len(qualityStart)
		qe := strings.Index(respString[qs:], qualityEnd) + qs

		fmt.Printf("resolution: %s, download with -quality=\"%s\"\n", respString[rs:re], respString[qs:qe])

		respString = respString[qe:]
	}
}

func wrongInputNotification() {
	fmt.Println("Call the program with -help for information on how to use it :^)")
}

func downloadPartVOD(vodIDString string, start string, end string, quality string, downloadPath string, filename string) {
	vodInfo := VodInfo2{
		Id:      vodIDString,
		BaseUrl: "",
		Pls:     "",
		Path:    "",
	}

	if *m3u8 != "" {
		m3u8List, err := ioutil.ReadFile(*m3u8)

		if err != nil {
			printFatal(err, "Could not open playlist")
		}

		//TODO: arbitrary format not supported yet
		vodInfo.Pls = string(m3u8List)
		//vodInfo.BaseUrl
		return
	} else {
		obtainMetadata(&vodInfo, quality)
	}

	fetchChunks(vodInfo, start, end, downloadPath, filename)
}

func obtainMetadata(vodInfo *VodInfo2, quality string) *string {
	var vodID int

	vodID, _ = strconv.Atoi(vodInfo.Id)

	tokenAPILink := fmt.Sprintf("https://api.twitch.tv/api/vods/%v/access_token?&client_id="+twitchClientID, vodID)

	fmt.Println("Contacting Twitch Server")

	sig, token, err := accessTokenAPI(tokenAPILink)
	if err != nil {
		printFatal(err, "Could not access twitch token api")
	}

	printDebugf("\nSig: %s, Token: %s\n", sig, token)

	usherAPILink := fmt.Sprintf("http://usher.twitch.tv/vod/%v?nauthsig=%v&nauth=%v&allow_source=true", vodID, sig, token)

	printDebugf("\nusherAPILink: %s\n", usherAPILink)

	edgecastURLmap, err := accessUsherAPI(usherAPILink)
	if err != nil {
		printFatal(err, "Couldn't access usher api")
	}

	printDebug(edgecastURLmap)

	// I don't see what this does. With this you can't download in source quality (chunked).
	// Fixed. But "chunked" playlist not always available, have to loop and find max quality manually

	m3u8Link, ok := edgecastURLmap[quality]

	if ok {
		fmt.Printf("Selected quality: %s\n", quality)
	} else {
		fmt.Printf("Couldn't find quality: %s\n", quality)

		// Try to find source quality playlist
		if quality != sourceQuality {
			quality = sourceQuality

			m3u8Link, ok = edgecastURLmap[quality]
		}

		if ok {
			fmt.Printf("Downloading in source quality: %s\n", quality)
		} else {
			// Quality still not matched
			resolutionMax := 0
			fpsMax := 0
			resolutionTmp := 0
			fpsTmp := 0
			var keyTmp []string

			// Find max quality
			for key := range edgecastURLmap {
				keyTmp = strings.Split(key, "p")

				resolutionTmp, _ = strconv.Atoi(keyTmp[0])

				if len(keyTmp) > 1 {
					fpsTmp, _ = strconv.Atoi(keyTmp[1])
				} else {
					fpsTmp = 0
				}

				if resolutionTmp > resolutionMax || resolutionTmp == resolutionMax && fpsTmp > fpsMax {
					quality = key
					fpsMax = fpsTmp
					resolutionMax = resolutionTmp
				}
			}

			m3u8Link, ok = edgecastURLmap[quality]

			if ok {
				fmt.Printf("Downloading in max available quality: %s\n", quality)
			} else {
				fmt.Println("No available quality options found")
				os.Exit(1)
			}
		}
	}
	edgecastBaseURL := m3u8Link
	if strings.Contains(edgecastBaseURL, edgecastLinkBaseEndOld) {
		edgecastBaseURL = edgecastBaseURL[0:strings.Index(edgecastBaseURL, edgecastLinkBaseEndOld)]
	} else {
		edgecastBaseURL = edgecastBaseURL[0:strings.Index(edgecastBaseURL, edgecastLinkBaseEnd)]
	}

	printDebugf("\nedgecastBaseURL: %s\nm3u8Link: %s\n", edgecastBaseURL, m3u8Link)

	fmt.Println("Getting Video info")

	m3u8List, err := getM3U8List(m3u8Link)

	if err != nil {
		printFatal(err, "Couldn't download m3u8 list")
	}

	vodInfo.Pls = m3u8List
	vodInfo.BaseUrl = edgecastBaseURL

	return &m3u8List
}

func fetchChunks(vodInfo VodInfo2, start string, end string, downloadPath string, filename string) {
	var vodSH, vodSM, vodSS, vodEH, vodEM, vodES int

	m3u8List := vodInfo.Pls

	vodSavePath := filepath.Join(downloadPath, filename+".mp4")

	_, err := os.Stat(vodSavePath)

	if err == nil || !os.IsNotExist(err) {
		printFatalf(err, "Destination file %s already exists!\n", vodSavePath)
	}

	startArray := strings.Split(start, " ")
	vodSH, _ = strconv.Atoi(startArray[0]) //start Hour
	vodSM, _ = strconv.Atoi(startArray[1]) //start minute
	vodSS, _ = strconv.Atoi(startArray[2]) //start second

	if end != "full" {
		endArray := strings.Split(end, " ")

		vodEH, _ = strconv.Atoi(endArray[0]) //end hour
		vodEM, _ = strconv.Atoi(endArray[1]) //end minute
		vodES, _ = strconv.Atoi(endArray[2]) //end second

		if toSeconds(vodSH, vodSM, vodSS) > toSeconds(vodEH, vodEM, vodES) {
			wrongInputNotification()
		}
	}

	printDebugf("\nm3u8List:\n%s\n", m3u8List)

	//this recently changed to relative uris, which is expected
	fileUris := readFileUris(m3u8List)
	//fileUris :=

	printDebugf("\nItems list: %v\n", fileUris)

	var chunkCount, startChunk int

	clipDuration := 0

	fileDurations, err := readFileDurations(m3u8List)

	if err != nil || len(fileDurations) != len(fileUris) {
		printDebug("Could not determine real file durations. Using targetDuration as fallback.")
		targetduration, _ := strconv.Atoi(m3u8List[strings.Index(m3u8List, targetdurationStart)+len(targetdurationStart) : strings.Index(m3u8List, targetdurationEnd)])
		chunkCount = calcChunkCount(vodSH, vodSM, vodSS, vodEH, vodEM, vodES, targetduration)
		startChunk = startingChunk(vodSH, vodSM, vodSS, targetduration)
	} else {
		startSeconds := toSeconds(vodSH, vodSM, vodSS)

		if end == "full" {
			sum := 0.0
			for _, val := range fileDurations {
				sum += val
			}
			clipDuration = int(sum - float64(startSeconds))
		} else {
			clipDuration = toSeconds(vodEH, vodEM, vodES) - startSeconds
		}

		startChunk, chunkCount, _ = calcStartChunkAndChunkCount(fileDurations, startSeconds, clipDuration)
	}

	printDebugf("\nchunkCount: %v\nstartChunk: %v\n", chunkCount, startChunk)

	var wg sync.WaitGroup
	wg.Add(chunkCount)

	newpath := filepath.Join(downloadPath, "_"+vodInfo.Id)

	vodInfo.Path = newpath

	err = os.MkdirAll(newpath, os.ModePerm)
	if err != nil {
		printFatal(err, "Could not create directory")
	}
	fmt.Printf("Created temp dir: %s\n", newpath)

	fmt.Println("Downloading")
	/*
		vodInfo := &VodInfo{
			Id:      vodIDString,
			BaseUrl: edgecastBaseURL,
			Path:    newpath,
		}
	*/

	jobs := make(chan ChunkInfo, chunkCount)
	status := make(chan int)

	for w := 1; w <= concurrentDownloads; w++ {
		go chunkWorker(vodInfo, jobs, status, &wg)
	}

	go downloadLogProgress(chunkCount, status)

	for i := startChunk; i < (startChunk + chunkCount); i++ {
		job := ChunkInfo{
			Id:  strconv.Itoa(i),
			Url: fileUris[i],
		}

		jobs <- job
	}

	wg.Wait()
	//FIXME: this sends null value to all workers
	//	close(jobs)

	fmt.Println("\nCombining parts")

	go ffmpegStatus(clipDuration)
	ffmpegCombine(newpath, chunkCount, startChunk, vodInfo.Id, vodSavePath)

	fmt.Println("\nDeleting chunks")

	deleteChunks(newpath, chunkCount, startChunk, vodInfo.Id)

	fmt.Println("Deleting temp dir")

	os.Remove(newpath)

	fmt.Println("Complete")
}

type VodInfo struct {
	Path    string
	BaseUrl string
	Id      string
}

type VodInfo2 struct {
	Path    string
	BaseUrl string
	Pls     string
	Id      string
}

type ChunkInfo struct {
	Id  string
	Url string
}

func chunkWorker(vod VodInfo2, chunkJobs <-chan ChunkInfo, status chan<- int, wg *sync.WaitGroup) {
	for {
		j := <-chunkJobs

		chunkLength := downloadChunk(vod.Path, vod.BaseUrl, j.Id, j.Url, vod.Id)

		if chunkLength > 0 {
			status <- 1
		} else {
			status <- 0
		}

		wg.Done()
	}
}

func downloadLogProgress(chunkCount int, status <-chan int) {
	var downloaded int = 0
	bar := progressbar.NewOptions(chunkCount,
		progressbar.OptionSetRenderBlankState(!debug),
		progressbar.OptionShowCount(),
		progressbar.OptionForceRender(),
	)
	//FIXME: this is a dirty hack for racing condition between wg and bar render
	bar.Add(1)

	for {
		downloaded += <-status

		if !debug {
			bar.Add(1)
			/*
				if downloaded == chunkCount {
					bar.Finish()
					break
				}
			*/
		}
	}
}

func calcStartChunkAndChunkCount(chunkDurations []float64, startSeconds int, clipDuration int) (int, int, float64) {
	startChunk := 0
	chunkCount := 0
	startSecondsRemainder := float64(0)

	cumulatedDuration := 0.0
	for chunk, chunkDuration := range chunkDurations {
		cumulatedDuration += chunkDuration

		if cumulatedDuration > float64(startSeconds) {
			startChunk = chunk
			startSecondsRemainder = float64(startSeconds) - (cumulatedDuration - chunkDuration)
			break
		}
	}

	cumulatedDuration = 0.0
	minChunkedClipDuration := float64(clipDuration) + startSecondsRemainder
	for chunk := startChunk; chunk < len(chunkDurations); chunk++ {
		cumulatedDuration += chunkDurations[chunk]

		if cumulatedDuration > minChunkedClipDuration {
			chunkCount = chunk - startChunk + 1
			break
		}
	}

	if chunkCount == 0 {
		chunkCount = len(chunkDurations) - startChunk
	}

	return startChunk, chunkCount, startSecondsRemainder
}

func readFileUris(m3u8List string) []string {
	var fileRegex = regexp.MustCompile("(?m:^[^#\\n]+)")
	matches := fileRegex.FindAllStringSubmatch(m3u8List, -1)
	var ret []string
	for _, match := range matches {
		ret = append(ret, match[0])
	}
	return ret
}

func readFileDurations(m3u8List string) ([]float64, error) {
	var fileRegex = regexp.MustCompile("(?m:^#EXTINF:(\\d+(\\.\\d+)?))")
	matches := fileRegex.FindAllStringSubmatch(m3u8List, -1)

	var ret []float64

	for _, match := range matches {

		fileLength, err := strconv.ParseFloat(match[1], 64)

		if err != nil {
			printDebug(err)
			return nil, err
		}

		ret = append(ret, fileLength)
	}

	return ret, nil
}

func ffmpegIsInstalled() bool {
	out, _ := exec.Command(ffmpegCMD).Output()
	return out != nil
}

func main() {

	qualityInfo := flag.Bool("qualityinfo", false, "if you want to see the avaliable quality options")

	standardVOD := "123456789"
	vodID := flag.String("vod", standardVOD, "the vod id https://www.twitch.tv/videos/123456789")
	start := flag.String("start", "0 0 0", "For example: 0 0 0 for starting at the beginning of the vod")
	end := flag.String("end", "full", "For example: 1 20 0 for ending the vod at 1 hour and 20 minutes")
	quality := flag.String("quality", sourceQuality, "chunked for source quality is automatically used if -quality isn't set")
	//	myClientID := flag.String("client-id", twitchClientID, "Use your own client id")
	debugFlag := flag.Bool("debug", false, "debug output")
	semaphoreLimit := flag.Int("max-concurrent-downloads", 5, "change maximum number of concurrent downloads")
	downloadPath := flag.String("download-path", ".", "path where the file will be saved")
	filename := flag.String("filename", "", "name of the output file (without extension)")
	audio = flag.Bool("audio", false, "extract audio from the video file")
	audioOnly = flag.Bool("audio-only", false, "end up only with a audio file")
	maxTryCount = flag.Int("try-count", 3, "amount of times concat should try fetching chunks. Set to 0 for infinite retries")

	m3u8 = flag.String("playlist", "", "external stream playlist")

	flag.Parse()

	if *filename == "" {
		filename = vodID
	}

	if !*qualityInfo && !ffmpegIsInstalled() {
		fmt.Println("Could not find ffmpeg, make sure to have ffmpeg avaliable on your system.")
		os.Exit(1)
	}

	debug = *debugFlag
	concurrentDownloads = *semaphoreLimit

	if *vodID == standardVOD {
		wrongInputNotification()
		os.Exit(1)
	}

	if *qualityInfo {
		printQualityOptions(*vodID)
		os.Exit(0)
	}

	downloadPartVOD(*vodID, *start, *end, *quality, *downloadPath, *filename)
}
