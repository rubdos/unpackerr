package unpacker

/* Folder Watching Codez */

import (
	"os"
	"path"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"golift.io/cnfg"
	"golift.io/xtractr"
)

const (
	updateChanSize   = 1000  // Size of update channel. This is sufficiently large.
	defaultQueueSize = 20000 // Channel queue size for file system events.
	minimumQueueSize = 1000  // The snallest size the channel buffer can be.
)

// FolderConfig defines the input data for a watched folder.
type FolderConfig struct {
	DeleteOrig  bool          `json:"delete_original" toml:"delete_original" xml:"delete_original" yaml:"delete_original"`
	MoveBack    bool          `json:"move_back" toml:"move_back" xml:"move_back" yaml:"move_back"`
	DeleteAfter cnfg.Duration `json:"delete_after" toml:"delete_after" xml:"delete_after" yaml:"delete_after"`
	Path        string        `json:"path" toml:"path" xml:"path" yaml:"path"`
}

// Folders holds all known (created) folders in all watch paths.
type Folders struct {
	Config  []*FolderConfig
	Folders map[string]*Folder
	Events  chan *eventData
	Updates chan *update
	Logf    func(msg string, v ...interface{})
	Debug   func(msg string, v ...interface{})
	Watcher *fsnotify.Watcher
}

// Folder is a "new" watched folder.
type Folder struct {
	last time.Time
	step ExtractStatus
	cnfg *FolderConfig
	list []string
}

type eventData struct {
	cnfg *FolderConfig
	name string
	file string
}

type update struct {
	Step ExtractStatus
	Name string
	Resp *xtractr.Response
}

func (u *Unpackerr) logFolders() {
	if c := len(u.Folders); c == 1 {
		u.Logf(" => Folder Config: 1 path: %s (delete after:%v, delete orig:%v, move back:%v, event buffer:%d)",
			u.Folders[0].Path, u.Folders[0].DeleteAfter, u.Folders[0].DeleteOrig, u.Folders[0].MoveBack, u.Buffer)
	} else {
		u.Log(" => Folder Config:", c, "paths,", "event buffer:", u.Buffer)

		for _, f := range u.Folders {
			u.Logf(" =>    Path: %s (delete after:%v, delete orig:%v, move back:%v)",
				f.Path, f.DeleteAfter, f.DeleteOrig, f.MoveBack)
		}
	}
}

// PollFolders begins the routines to watch folders for changes.
// if those changes include the addition of compressed files, they
// are processed for exctraction.
func (u *Unpackerr) PollFolders() {
	var (
		flist []string
		err   error
	)

	u.Folders, flist = u.checkFolders()

	if u.folders, err = u.newFolderWatcher(); err != nil {
		u.Log("[ERROR] Watching Folders:", err)

		return
	}
	// do not close the watcher.

	if len(u.Folders) == 0 {
		return
	}

	u.Log("[Folder] Watching:", strings.Join(flist, ", "))

	go u.folders.watchFSNotify()
}

// newFolderWatcher returns a new folder watcher.
// You must call folders.Watcher.Close() when you're done with it.
func (u *Unpackerr) newFolderWatcher() (*Folders, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	for _, folder := range u.Folders {
		if err := watcher.Add(folder.Path); err != nil {
			u.Log("[ERROR] Folder (cannot watch):", err)
		}
	}

	return &Folders{
		Config:  u.Folders,
		Folders: make(map[string]*Folder),
		Events:  make(chan *eventData, u.Config.Buffer),
		Updates: make(chan *update, updateChanSize),
		Debug:   u.Debug,
		Logf:    u.Logf,
		Watcher: watcher,
	}, nil
}

// checkFolders stats all configured folders and returns only "good" ones.
func (u *Unpackerr) checkFolders() ([]*FolderConfig, []string) {
	goodFolders := []*FolderConfig{}
	goodFlist := []string{}

	for _, f := range u.Folders {
		if stat, err := os.Stat(f.Path); err != nil {
			u.Log("[ERROR] Folder (cannot watch):", err)

			continue
		} else if !stat.IsDir() {
			u.Logf("[ERROR] Folder (cannot watch): %s: not a folder", f.Path)

			continue
		}

		f.Path = strings.TrimSuffix(f.Path, "/") + "/"
		goodFolders = append(goodFolders, f)
		goodFlist = append(goodFlist, f.Path)
	}

	return goodFolders, goodFlist
}

// extractFolder starts a folder's extraction after it hasn't been written to in a while.
func (u *Unpackerr) extractFolder(name string, folder *Folder) {
	// update status.
	_ = u.folders.Watcher.Remove(name)
	u.folders.Folders[name].last = time.Now()
	u.folders.Folders[name].step = QUEUED
	// create a queue counter in the main history.
	u.updateQueueStatus(&Extracts{Path: name, Status: QUEUED})

	// extract it.
	queueSize, err := u.Extract(&xtractr.Xtract{
		Name:       name,
		SearchPath: name,
		TempFolder: !folder.cnfg.MoveBack,
		DeleteOrig: false,
		CBFunction: u.folders.xtractCallback,
	})
	if err != nil {
		u.Log("[ERROR]", err)

		return
	}

	u.Logf("[Folder] Queued: %s, queue size: %d", name, queueSize)
}

func (u *Unpackerr) processFolderUpdate(update *update) {
	if _, ok := u.folders.Folders[update.Name]; !ok {
		// It doesn't exist? weird. delete it and bail out.
		delete(u.Map, update.Name)

		return
	}

	u.updateQueueStatus(&Extracts{Path: update.Name, Status: update.Step})
	u.folders.Folders[update.Name].last = time.Now()
	u.folders.Folders[update.Name].step = update.Step

	// Resp is only set when the extraction is finished.
	if update.Resp != nil {
		u.folders.Folders[update.Name].list = update.Resp.NewFiles
	}
}

// watchFSNotify reads file system events from a channel and processes them.
func (f *Folders) watchFSNotify() {
	for {
		select {
		case err, ok := <-f.Watcher.Errors:
			if !ok {
				return
			}

			f.Logf("[ERROR] fsnotify: %v", err)
		case event, ok := <-f.Watcher.Events:
			if !ok {
				return
			}

			if strings.HasSuffix(event.Name, suffix) {
				break
			}

			for _, cnfg := range f.Config {
				// Find the configured folder for the event we just got.
				if !strings.HasPrefix(event.Name, cnfg.Path) {
					continue
				}

				// cnfg.Path: "/Users/Documents/auto"
				// event.Name: "/Users/Documents/auto/my_folder/file.rar"
				// p: "my_folder"
				p := strings.TrimPrefix(event.Name, cnfg.Path)
				if np := path.Dir(p); np != "." {
					p = np
				}
				// Send this event to processEvent().
				f.Events <- &eventData{name: p, cnfg: cnfg, file: path.Base(event.Name)}
			}
		}
	}
}

// processEvent processes the event that was received.
func (f *Folders) processEvent(event *eventData) {
	fullPath := path.Join(event.cnfg.Path, event.name)
	if stat, err := os.Stat(fullPath); err != nil {
		// Item is unusable (probably deleted), remove it from history.
		if _, ok := f.Folders[fullPath]; ok {
			f.Debug("Folder: Removing Tracked Item: %v", fullPath)
			delete(f.Folders, fullPath)
			_ = f.Watcher.Remove(fullPath)
		}

		return
	} else if !stat.IsDir() {
		f.Debug("Folder: Ignoring Item: %v (not a folder)", fullPath)

		return
	}

	if _, ok := f.Folders[fullPath]; ok {
		//		f.DeLogf("Item Updated: %v (file: %v)", fullPath, event.file)
		f.Folders[fullPath].last = time.Now()

		return
	}

	if err := f.Watcher.Add(fullPath); err != nil {
		f.Logf("[ERROR] Folder: Tracking New Item: %v: %v", fullPath, err)

		return
	}

	f.Logf("[Folder] Tracking New Item: %v", fullPath)

	f.Folders[fullPath] = &Folder{
		last: time.Now(),
		step: WAITING,
		cnfg: event.cnfg,
	}
}

// checkFolderStats runs at an interval to see if any folders need work done on them.
func (u *Unpackerr) checkFolderStats() {
	for name, folder := range u.folders.Folders {
		switch elapsed := time.Since(folder.last); {
		case EXTRACTFAILED == folder.step && elapsed >= u.RetryDelay.Duration:
			u.Logf("[Folder] Re-starting Failed Extraction: %s (failed %v ago)",
				folder.cnfg.Path, elapsed.Round(time.Second))

			folder.last = time.Now()
			folder.step = WAITING
			u.Restarted++
		case EXTRACTED == folder.step && elapsed >= folder.cnfg.DeleteAfter.Duration:
			// Folder reached delete delay (after extraction), nuke it.
			u.updateQueueStatus(&Extracts{Path: name, Status: DELETED})
			delete(u.folders.Folders, name)

			// Only delete the extracted files if DeleteAfter is greater than 0.
			if !folder.cnfg.MoveBack && folder.cnfg.DeleteAfter.Duration > 0 {
				u.DeleteFiles(strings.TrimRight(name, `/\`) + suffix)
			}

			if folder.cnfg.DeleteOrig {
				u.DeleteFiles(name)
			}
		case WAITING == folder.step && elapsed >= u.StartDelay.Duration:
			// The folder hasn't been written to in a while, extract it.
			u.extractFolder(name, folder)
		}
	}
}

// updateQueueStatus for an on-going tracked extraction.
// This is called from a channel callback to update status in a single go routine.
func (u *Unpackerr) updateQueueStatus(data *Extracts) {
	if _, ok := u.Map[data.Path]; ok {
		u.Map[data.Path].Status = data.Status
		u.Map[data.Path].Files = append(u.Map[data.Path].Files, data.Files...)
		u.Map[data.Path].Updated = time.Now()

		return
	}

	// This is a new folder being extracted.
	data.Updated = time.Now()
	data.Status = QUEUED
	u.Map[data.Path] = data
}

// xtractCallback is run twice by the xtractr library when the extraction begins, and finishes.
func (f *Folders) xtractCallback(resp *xtractr.Response) {
	switch {
	case !resp.Done:
		f.Logf("[Folder] Extraction Started: %s, items in queue: %d", resp.X.Name, resp.Queued)
		f.Updates <- &update{Step: EXTRACTING, Name: resp.X.Name}
	case resp.Error != nil:
		f.Logf("[Folder] Extraction Error: %s: %v", resp.X.Name, resp.Error)
		f.Updates <- &update{Step: EXTRACTFAILED, Name: resp.X.Name}
	default: // this runs in a go routine
		f.Logf("[Folder] Extraction Finished: %s => elapsed: %v, archives: %d, "+
			"extra archives: %d, files extracted: %d, written: %dMiB",
			resp.X.Name, resp.Elapsed.Round(time.Second), len(resp.Archives),
			len(resp.Extras), len(resp.AllFiles), resp.Size/mebiByte)
		// Send the update back into our channel (single go routine) to processFolderUpdate().
		f.Updates <- &update{Step: EXTRACTED, Resp: resp, Name: resp.X.Name}
	}
}
