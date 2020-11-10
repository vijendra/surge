package surge

import (
	"bufio"
	"fmt"
	"math/rand"
	"net"
	"os"
	"os/user"
	"runtime"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	bitmap "github.com/boljen/go-bitmap"
	movavg "github.com/mxmCherry/movavg"
	nkn "github.com/nknorg/nkn-sdk-go"
	dialog "github.com/sqweek/dialog"
	"github.com/wailsapp/wails"
)

// SurgeActive is true when client is operational
var SurgeActive bool = false

//ChunkSize is size of chunk in bytes (256 kB)
const ChunkSize = 1024 * 256

//NumClients is the number of NKN clients
const NumClients = 8

//NumWorkers is the total number of concurrent chunk fetches allowed
const NumWorkers = 16

const localPath = "local"
const remotePath = "remote"

var localFolder = ""
var remoteFolder = ""
var magnetstring = ""
var filestring = ""
var mode = ""

var subscribers []string

//OS folder permission bitflags
const (
	osRead       = 04
	osWrite      = 02
	osEx         = 01
	osUserShift  = 6
	osGroupShift = 3
	osOthShift   = 0

	osUserR   = osRead << osUserShift
	osUserW   = osWrite << osUserShift
	osUserX   = osEx << osUserShift
	osUserRw  = osUserR | osUserW
	osUserRwx = osUserRw | osUserX

	osGroupR   = osRead << osGroupShift
	osGroupW   = osWrite << osGroupShift
	osGroupX   = osEx << osGroupShift
	osGroupRw  = osGroupR | osGroupW
	osGroupRwx = osGroupRw | osGroupX

	osOthR   = osRead << osOthShift
	osOthW   = osWrite << osOthShift
	osOthX   = osEx << osOthShift
	osOthRw  = osOthR | osOthW
	osOthRwx = osOthRw | osOthX

	osAllR   = osUserR | osGroupR | osOthR
	osAllW   = osUserW | osGroupW | osOthW
	osAllX   = osUserX | osGroupX | osOthX
	osAllRw  = osAllR | osAllW
	osAllRwx = osAllRw | osGroupX
)

var localFileName string
var sendSize int64
var receivedSize int64

var startTime = time.Now()

var client *nkn.MultiClient

var clientOnlineMap map[string]bool

var downloadBandwidthAccumulator map[string]int
var uploadBandwidthAccumulator map[string]int

var fileBandwidthMap map[string]BandwidthMA

var zeroBandwidthMap map[string]bool

var clientOnlineMapLock = &sync.Mutex{}
var bandwidthAccumulatorMap = &sync.Mutex{}

//Sessions .
var Sessions []*Session

//var testReader *bufio.Reader

var workerCount = 0

//Sessions collection lock
var sessionsWriteLock = &sync.Mutex{}

// File holds all info of a tracked file in surge
type File struct {
	FileName      string
	FileSize      int64
	FileHash      string
	Seeder        string
	Path          string
	NumChunks     int
	IsDownloading bool
	IsUploading   bool
	IsPaused      bool
	IsMissing     bool
	ChunkMap      []byte
}

type NumClientsStruct struct {
	Subscribed int
	Online     int
}

// FileListing struct for all frontend file listing props
type FileListing struct {
	FileName    string
	FileSize    int64
	FileHash    string
	Seeder      string
	NumChunks   int
	IsTracked   bool
	IsAvailable bool
}

// Session is a wrapper for everything needed to maintain a surge session
type Session struct {
	FileHash   string
	FileSize   int64
	Downloaded int64
	Uploaded   int64
	session    net.Conn
	reader     *bufio.Reader
	file       *os.File
}

// FileStatusEvent holds update info on download progress
type FileStatusEvent struct {
	FileHash          string
	Progress          float32
	Status            string
	DownloadBandwidth int
	UploadBandwidth   int
	NumChunks         int
	ChunkMap          string
}

//BandwidthMA tracks moving average for download and upload bandwidth
type BandwidthMA struct {
	Download movavg.MA
	Upload   movavg.MA
}

//ListedFiles are remote files that can be downloaded
var ListedFiles []File

var wailsRuntime *wails.Runtime

var labelText chan string
var appearance chan string

var numClientsSubscribed int = 0
var numClientsOnline int = 0

var numClientsStore *wails.Store

// Start initializes surge
func Start(runtime *wails.Runtime, args []string) {
	var err error

	//Mac specific functions
	go initOSHandler()
	go setVisualModeLikeOS()

	wailsRuntime = runtime

	numClients := NumClientsStruct{
		Subscribed: 0,
		Online:     0,
	}

	numClientsStore = wailsRuntime.Store.New("numClients", numClients)

	var dirFileMode os.FileMode
	var dir = GetSurgeDir()
	dirFileMode = os.ModeDir | (osUserRwx | osAllR)

	myself, err := user.Current()
	if err != nil {
		pushError("Error on startup", err.Error())
	}
	homedir := myself.HomeDir
	localFolder = homedir + string(os.PathSeparator) + "Downloads" + string(os.PathSeparator) + "surge_" + localPath
	remoteFolder = homedir + string(os.PathSeparator) + "Downloads" + string(os.PathSeparator) + "surge_" + remotePath

	if _, err := os.Stat(dir); os.IsNotExist(err) {
		// seems like this is the first time starting the app
		//set tour to active
		DbWriteSetting("Tour", "true")
		//set default mode to light
		DbWriteSetting("DarkMode", "false")

		os.Mkdir(dir, dirFileMode)
	}

	//Ensure local and remote folders exist
	if _, err := os.Stat(localFolder); os.IsNotExist(err) {
		os.Mkdir(localFolder, dirFileMode)
	}
	if _, err := os.Stat(remoteFolder); os.IsNotExist(err) {
		os.Mkdir(remoteFolder, dirFileMode)
	}

	account := InitializeAccount()
	client, err = nkn.NewMultiClient(account, "", NumClients, false, nil)

	if err != nil {
		pushError("Error on startup", err.Error())
	} else {
		log.Println("MY ADDRESS:", client.Addr().String())
		<-client.OnConnect.C

		pushNotification("Client Connected", "Successfully connected to the NKN network")

		client.Listen(nil)
		SurgeActive = true
		go Listen()

		topicEncoded := TopicEncode(TestTopic)

		clientOnlineMap = make(map[string]bool)
		downloadBandwidthAccumulator = make(map[string]int)
		uploadBandwidthAccumulator = make(map[string]int)
		zeroBandwidthMap = make(map[string]bool)
		fileBandwidthMap = make(map[string]BandwidthMA)

		dbFiles := dbGetAllFiles()
		var filesOnDisk []File

		for i := 0; i < len(dbFiles); i++ {
			if FileExists(dbFiles[i].Path) {
				filesOnDisk = append(filesOnDisk, dbFiles[i])
			} else {
				dbFiles[i].IsMissing = true
				dbFiles[i].IsDownloading = false
				dbFiles[i].IsUploading = false
				dbInsertFile(dbFiles[i])
			}
		}

		go BuildSeedString(filesOnDisk)
		for i := 0; i < len(filesOnDisk); i++ {
			go restartDownload(filesOnDisk[i].FileHash)
		}

		go sendSeedSubscription(topicEncoded, "Surge File Seeder")
		go GetSubscriptions(topicEncoded)

		go updateGUI()

		go rescanPeers()
		go queryRemoteForFiles()

		go watchOSXHandler()

		//Insert new file from arguments and start download
		if args != nil && len(args) > 0 && len(args[0]) > 0 {
			askUser("startDownloadMagnetLinks", "{files : ["+args[0]+"]}")
		}

		//Just paste one of your own magnets (from the startup logs) here to download something over nkn from yourself to test if no-one is online
		//go ParsePayloadString("surge://|file|justatvshow.mp4|219091405|cd0731496277102a869dacb0e99b7708c2b708824b647ffeb267de4743b7856e|a536528d2e321623375535af88974d7a7899836f9b84644320023bc3af3b9cf1|/")
	}
}

func rescanPeers() {
	for true {
		var numOnline = 0
		//Count num online clients
		clientOnlineMapLock.Lock()
		for _, value := range clientOnlineMap {
			if value == true {
				numOnline++
			}
		}

		numClientsSubscribed = len(clientOnlineMap)
		numClientsOnline = numOnline

		numClientsStore.Update(func(data NumClientsStruct) NumClientsStruct {
			return NumClientsStruct{
				Subscribed: len(clientOnlineMap),
				Online:     numOnline,
			}
		})

		clientOnlineMapLock.Unlock()

		time.Sleep(time.Minute)
		topicEncoded := TopicEncode(TestTopic)
		go GetSubscriptions(topicEncoded)
	}
}

func queryRemoteForFiles() {
	for true {
		for _, address := range subscribers {
			clientOnlineMapLock.Lock()
			clientOnlineMap[address] = false
			clientOnlineMapLock.Unlock()
			go SendQueryRequest(address, "Testing query functionality.")
			time.Sleep(time.Second * 5)
		}
	}
}

//GetNumberOfRemoteClient returns number of clients and online clients
func GetNumberOfRemoteClient() (int, int) {
	return numClientsSubscribed, numClientsOnline
}

func updateGUI() {
	for true {
		time.Sleep(time.Second)

		//Create session aggregate maps for file
		fileProgressMap := make(map[string]float32)

		sessionsWriteLock.Lock()
		for _, session := range Sessions {
			//log.Println("Active session:", session.session.RemoteAddr().String())
			if session.FileSize == 0 {
				continue
			}

			fileProgressMap[session.FileHash] = float32(float64(session.Downloaded) / float64(session.FileSize))

			if session.Downloaded == session.FileSize {
				showNotification("Download Finished", "Download for "+getListedFileByHash(session.FileHash).FileName+" finished!")
				pushNotification("Download Finished", getListedFileByHash(session.FileHash).FileName)
				session.session.Close()

				fileEntry, err := dbGetFile(session.FileHash)
				if err != nil {
					pushError("Error on download complete", err.Error())
				}
				fileEntry.IsDownloading = false
				fileEntry.IsUploading = true
				dbInsertFile(*fileEntry)
				go AddToSeedString(*fileEntry)
			}
		}
		sessionsWriteLock.Unlock()

		totalDown := 0
		totalUp := 0

		//Insert uploads
		allFiles := dbGetAllFiles()
		for _, file := range allFiles {
			if file.IsUploading {
				fileProgressMap[file.FileHash] = 1
			}
			key := file.FileHash

			if file.IsPaused {
				continue
			}

			down, up := fileBandwidth(key)
			totalDown += down
			totalUp += up

			if zeroBandwidthMap[key] == false || down+up != 0 {
				statusEvent := FileStatusEvent{
					FileHash:          key,
					Progress:          fileProgressMap[key],
					DownloadBandwidth: down,
					UploadBandwidth:   up,
					NumChunks:         file.NumChunks,
					ChunkMap:          GetFileChunkMapString(&file, 400),
				}
				wailsRuntime.Events.Emit("fileStatusEvent", statusEvent)
			}

			zeroBandwidthMap[key] = down+up == 0
		}

		//log.Println("Emitting globalBandwidthUpdate: ", totalDown, totalUp)
		if zeroBandwidthMap["total"] == false || totalDown+totalUp != 0 {
			wailsRuntime.Events.Emit("globalBandwidthUpdate", totalDown, totalUp)
		}

		zeroBandwidthMap["total"] = totalDown+totalUp == 0
	}
}

func fileBandwidth(FileID string) (Download int, Upload int) {
	//Get accumulator
	bandwidthAccumulatorMap.Lock()
	downAccu := downloadBandwidthAccumulator[FileID]
	downloadBandwidthAccumulator[FileID] = 0

	upAccu := uploadBandwidthAccumulator[FileID]
	uploadBandwidthAccumulator[FileID] = 0
	bandwidthAccumulatorMap.Unlock()

	if fileBandwidthMap[FileID].Download == nil {
		fileBandwidthMap[FileID] = BandwidthMA{
			Download: movavg.ThreadSafe(movavg.NewSMA(10)),
			Upload:   movavg.ThreadSafe(movavg.NewSMA(10)),
		}
	}

	fileBandwidthMap[FileID].Download.Add(float64(downAccu))
	fileBandwidthMap[FileID].Upload.Add(float64(upAccu))

	return int(fileBandwidthMap[FileID].Download.Avg()), int(fileBandwidthMap[FileID].Upload.Avg())

	//Take bandwith delta
	/*deltaDownload := int(Session.Downloaded - Session.deltaDownloaded)
	Session.deltaDownloaded = Session.Downloaded
	deltaUpload := int(Session.Uploaded - Session.deltaUploaded)
	Session.deltaUploaded = Session.Uploaded

	//Append to queue
	Session.bandwidthDownloadQueue = append(Session.bandwidthDownloadQueue, deltaDownload)
	Session.bandwidthUploadQueue = append(Session.bandwidthUploadQueue, deltaUpload)

	//Dequeue if queue > 10
	if len(Session.bandwidthDownloadQueue) > 10 {
		Session.bandwidthDownloadQueue = Session.bandwidthDownloadQueue[1:]
		Session.bandwidthUploadQueue = Session.bandwidthUploadQueue[1:]
	}

	var downloadMA10 = 0
	var uploadMA10 = 0
	for i := 0; i < len(Session.bandwidthDownloadQueue); i++ {
		downloadMA10 += Session.bandwidthDownloadQueue[i]
		uploadMA10 += Session.bandwidthUploadQueue[i]
	}
	downloadMA10 /= len(Session.bandwidthDownloadQueue)
	uploadMA10 /= len(Session.bandwidthUploadQueue)

	return downloadMA10, uploadMA10*/
}

//ByteCountSI converts filesize in bytes to human readable text
func ByteCountSI(b int64) string {
	const unit = 1000
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB",
		float64(b)/float64(div), "kMGTPE"[exp])
}

func getFileSize(path string) (size int64) {
	fi, err := os.Stat(path)
	if err != nil {
		return -1
	}
	// get the size
	return fi.Size()
}

func sendSeedSubscription(Topic string, Payload string) {
	txnHash, err := client.Subscribe("", Topic, 4320, Payload, nil)
	if err != nil {
		log.Println("Probably already subscribed", err)
	} else {
		log.Println("Subscribed: ", txnHash)
	}
}

//GetSubscriptions .
func GetSubscriptions(Topic string) {
	subResponse, err := client.GetSubscribers(Topic, 0, 100, true, true)
	if err != nil {
		pushError("Error on get subscriptions", err.Error())
		return
	}

	for k, v := range subResponse.SubscribersInTxPool.Map {
		subResponse.Subscribers.Map[k] = v
	}

	subscribers = []string{}
	for k, v := range subResponse.Subscribers.Map {
		if len(v) > 0 {
			if k != client.Addr().String() {
				subscribers = append(subscribers, k)
			}
		}
	}
}

// Stats .
type Stats struct {
	log *wails.CustomLogger
}

// WailsInit .
func (s *Stats) WailsInit(runtime *wails.Runtime) error {
	s.log = runtime.Log.New("Stats")
	runtime.Events.Emit("notificationEvent", "Backend Init", "just a test")
	return nil
}

func getListedFileByHash(Hash string) *File {
	for _, file := range ListedFiles {
		if file.FileHash == Hash {
			return &file
		}
	}
	return nil
}

//DownloadFile downloads the file
func DownloadFile(Hash string) bool {
	//Addr string, Size int64, FileID string

	file := getListedFileByHash(Hash)
	if file == nil {
		pushError("Error on download file", "No listed file with hash: "+Hash)
	}

	// Create a sessions
	surgeSession, err := createSession(file)
	if err != nil {
		log.Println("Could not create session for download", Hash)
		pushNotification("Download Session Failed", file.FileName)
		return false
	}
	go initiateSession(surgeSession)

	pushNotification("Download Started", file.FileName)

	// If the file doesn't exist allocate it
	var path = remoteFolder + string(os.PathSeparator) + file.FileName
	fmt.Println(path)
	fmt.Println(path)
	fmt.Println(path)
	AllocateFile(path, file.FileSize)
	numChunks := int((file.FileSize-1)/int64(ChunkSize)) + 1

	//When downloading from remote enter file into db
	dbFile, err := dbGetFile(Hash)
	log.Println(dbFile)
	if err != nil {
		file.Path = path
		file.NumChunks = numChunks
		file.ChunkMap = bitmap.NewSlice(numChunks)
		file.IsDownloading = true
		dbInsertFile(*file)
	}

	//Create a random fetch sequence
	randomChunks := make([]int, numChunks)
	for i := 0; i < numChunks; i++ {
		randomChunks[i] = i
	}
	rand.Seed(time.Now().UnixNano())
	rand.Shuffle(len(randomChunks), func(i, j int) { randomChunks[i], randomChunks[j] = randomChunks[j], randomChunks[i] })

	downloadJob := func() {
		for i := 0; i < numChunks; i++ {
			//Pause if file is paused
			dbFile, err := dbGetFile(file.FileHash)
			for err == nil && dbFile.IsPaused {
				time.Sleep(time.Second * 5)
				dbFile, err = dbGetFile(file.FileHash)
				if err != nil {
					break
				}
			}

			workerCount++
			go RequestChunk(surgeSession, file.FileHash, int32(randomChunks[i]))

			for workerCount >= NumWorkers {
				time.Sleep(time.Millisecond)
			}
		}
	}
	go downloadJob()

	return true
}

func pushNotification(title string, text string) {
	//log.Println("Emitting Event: ", "notificationEvent", title, text)
	wailsRuntime.Events.Emit("notificationEvent", title, text)
}

func askUser(context string, payload string) {
	//log.Println("Emitting Event: ", "notificationEvent", title, text)
	wailsRuntime.Events.Emit("userEvent", context, payload)
}

func pushError(title string, text string) {
	//log.Println("Emitting Event: ", "errorEvent", title, text)
	wailsRuntime.Events.Emit("errorEvent", title, text)
}

//SearchQueryResult is a paging query result for file searches
type SearchQueryResult struct {
	Result []FileListing
	Count  int
}

//LocalFilePageResult is a paging query result for tracked files
type LocalFilePageResult struct {
	Result []File
	Count  int
}

//SearchFile runs a paged query
func SearchFile(Query string, Skip int, Take int) SearchQueryResult {
	var results []FileListing

	for _, file := range ListedFiles {
		if strings.Contains(strings.ToLower(file.FileName), strings.ToLower(Query)) {

			result := FileListing{
				FileName:  file.FileName,
				FileHash:  file.FileHash,
				FileSize:  file.FileSize,
				Seeder:    file.Seeder,
				NumChunks: file.NumChunks,
			}

			tracked, err := dbGetFile(result.FileHash)
			if err == nil && tracked != nil {
				result.IsTracked = true
				result.IsAvailable = true

				//If any chunk is missing set available to false
				for i := 0; i < result.NumChunks; i++ {
					if bitmap.Get(tracked.ChunkMap, i) == false {
						result.IsAvailable = false
						break
					}
				}
			}

			results = append(results, result)
		}
	}

	left := Skip
	right := Skip + Take

	if left > len(results) {
		left = len(results)
	}

	if right > len(results) {
		right = len(results)
	}

	return SearchQueryResult{
		Result: results[left:right],
		Count:  len(results),
	}
}

//GetTrackedFiles returns all files tracked in surge client
func GetTrackedFiles() []File {
	return dbGetAllFiles()
}

//GetFileChunkMapString returns the chunkmap in hex for a file given by hash
func GetFileChunkMapString(file *File, Size int) string {
	outputSize := Size
	inputSize := file.NumChunks

	stepSize := float64(inputSize) / float64(outputSize)
	stepSizeInt := int(stepSize)

	var boolBuffer = ""
	if inputSize >= outputSize {

		for i := 0; i < outputSize; i++ {
			localCount := 0
			for j := 0; j < stepSizeInt; j++ {
				local := bitmap.Get(file.ChunkMap, int(float64(i)*stepSize)+j)
				if local {
					localCount++
				} else {
					boolBuffer += "0"
					break
				}
			}
			if localCount == stepSizeInt {
				boolBuffer += "1"
			}
		}
	} else {
		iter := float64(0)
		for i := 0; i < outputSize; i++ {
			local := bitmap.Get(file.ChunkMap, int(iter))
			if local {
				boolBuffer += "1"
			} else {
				boolBuffer += "0"
			}
			iter += stepSize
		}
	}
	return boolBuffer
}

//GetFileChunkMapStringByHash returns the chunkmap in hex for a file given by hash
func GetFileChunkMapStringByHash(Hash string, Size int) string {
	file, err := dbGetFile(Hash)
	if err != nil {
		return ""
	}
	return GetFileChunkMapString(file, 400)
}

//SetFilePause sets a file IsPaused state for by file hash
func SetFilePause(Hash string, State bool) {
	fileWriteLock.Lock()
	file, err := dbGetFile(Hash)
	if err != nil {
		pushNotification("Failed To Pause", "Could not find the file to pause.")
		return
	}
	file.IsPaused = State
	dbInsertFile(*file)
	fileWriteLock.Unlock()

	msg := "Paused"
	if State == false {
		msg = "Resumed"
	}
	pushNotification("Download "+msg, file.FileName)
}

//OpenFileDialog uses platform agnostic package for a file dialog
func OpenFileDialog() (string, error) {
	return dialog.File().Load()
}

//RemoveFile removes file from surge db and optionally from disk
func RemoveFile(Hash string, FromDisk bool) bool {

	//Close sessions for this file
	for _, session := range Sessions {
		if session.FileHash == Hash {
			closeSession(session)
			break
		}
	}

	fileWriteLock.Lock()

	if FromDisk {
		file, err := dbGetFile(Hash)
		if err != nil {
			log.Println("Error on remove file (read db)", err.Error())
			pushError("Error on remove file (read db)", err.Error())
			return false
		}
		err = os.Remove(file.Path)
		if err != nil {
			log.Println("Error on remove file (remove from disk)", err.Error())
			pushError("Error on remove file (remove from disk)", err.Error())
			return false
		}
	}

	err := dbDeleteFile(Hash)
	if err != nil {
		log.Println("Error on remove file (read db)", err.Error())
		pushError("Error on remove file (read db)", err.Error())
		return false
	}
	fileWriteLock.Unlock()

	//Rebuild entirely
	dbFiles := dbGetAllFiles()
	go BuildSeedString(dbFiles)

	return true
}

//GetSurgeDir returns the surge dir
func GetSurgeDir() string {
	if runtime.GOOS == "windows" {
		return os.Getenv("APPDATA") + string(os.PathSeparator) + "Surge"
	}
	return os.Getenv("HOME") + string(os.PathSeparator) + ".surge"
}
