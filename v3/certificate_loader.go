package luddite

import (
	"crypto/tls"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	log "github.com/sirupsen/logrus"
)

const (
	dedupDelay = 5 * time.Second
)

type CertificateLoader interface {
	GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error)
	Close() error
}

type certLoader struct {
	cert         atomic.Pointer[tls.Certificate]
	certFilePath string
	keyFilePath  string
	watcher      Watcher
	log          *log.Logger
}

func NewCertificateLoader(config *ServiceConfig, logger *log.Logger) (CertificateLoader, error) {
	cl := &certLoader{
		certFilePath: config.Transport.CertFilePath,
		keyFilePath:  config.Transport.KeyFilePath,
		log:          logger,
	}
	var err error
	if err = cl.storeCertificate(); err != nil {
		return nil, err
	}
	if config.Transport.ReloadOnUpdate {
		if cl.watcher, err = NewWatcher(cl.certFilePath, cl.keyFilePath, logger); err != nil {
			return nil, err
		}
		go cl.watcher.Watch(cl.storeCertificate)
	}
	return cl, nil
}

func (l *certLoader) storeCertificate() error {
	l.log.Debugf("storing cert: '%s', key: '%s'", l.certFilePath, l.keyFilePath)
	cert, err := tls.LoadX509KeyPair(l.certFilePath, l.keyFilePath)
	if err != nil {
		return fmt.Errorf("failed to load certificate '%s': '%s'", l.certFilePath, err)
	}
	l.cert.Store(&cert)
	return nil
}

func (l *certLoader) GetCertificate(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
	return l.cert.Load(), nil
}

func (l *certLoader) Close() error {
	if l.watcher != nil {
		return l.watcher.Close()
	}
	return nil
}

type Watcher interface {
	Close() error
	Watch(loadCertCallback func() error)
}

type watcher struct {
	*fsnotify.Watcher
	watchPaths []WatchPath
	log        *log.Logger
}

func NewWatcher(certFilePath, keyFilePath string, logger *log.Logger) (Watcher, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	certFileDir := path.Dir(certFilePath)
	if err = w.Add(certFileDir); err != nil {
		return nil, fmt.Errorf("error adding dir '%s' to watcher: %s", certFileDir, err.Error())
	}
	logger.Debugf("cert directory '%s' added to watcher", certFileDir)

	keyFileDir := path.Dir(keyFilePath)
	if keyFileDir != certFileDir {
		if err = w.Add(keyFileDir); err != nil {
			return nil, fmt.Errorf("error adding dir '%s' to watcher: %s", keyFileDir, err.Error())
		}
		logger.Debugf("key directory '%s' added to watcher", keyFileDir)
	}
	watcher := &watcher{
		Watcher: w,
		log:     logger,
	}
	var wp WatchPath
	for _, fp := range []string{certFilePath, keyFilePath} {
		wp, err = NewWatchPath(fp, logger)
		if err != nil {
			return nil, err
		}
		watcher.watchPaths = append(watcher.watchPaths, wp)
		logger.Debugf("added path '%s' to watcher", fp)
	}
	return watcher, nil
}

func (w *watcher) Close() error {
	if w.Watcher != nil {
		return w.Watcher.Close()
	}
	return nil
}

func (w *watcher) Watch(loadCertCallback func() error) {
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		select {
		case event, ok := <-w.Events:
			if !ok {
				return
			}
			if updated, err := w.handleEvent(event); err != nil {
				w.log.WithError(err).Error("error handling fs event")
			} else if updated {
				// N.B. process the event after a delay to avoid duplicates when both files are written
				timer = setDeDupTimer(timer, func() {
					if err = loadCertCallback(); err != nil {
						w.log.WithError(err).Error("error reloading certificate")
						return
					}
					for _, wp := range w.watchPaths {
						if err = wp.StoreModTime(); err != nil {
							w.log.WithError(err).Errorf("error updating watch path: %s", wp)
						}
					}
				})
			}
		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			w.log.WithError(err).Error("certificate watcher error")
		}
	}
}

func (w *watcher) handleEvent(event fsnotify.Event) (bool, error) {
	updated := false
	if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
		for _, wp := range w.watchPaths {
			updated = updated || wp.IsUpdated()
		}
	}
	return updated, nil
}

func setDeDupTimer(timer *time.Timer, callback func()) *time.Timer {
	if timer == nil {
		timer = time.AfterFunc(dedupDelay, callback)
	} else {
		timer.Reset(dedupDelay)
	}
	return timer
}

type WatchPath interface {
	IsUpdated() bool
	StoreModTime() error
}

type watchPath struct {
	path    string
	modTime atomic.Pointer[time.Time]
	log     *log.Logger
}

func NewWatchPath(p string, logger *log.Logger) (WatchPath, error) {
	wp := &watchPath{
		path: p,
		log:  logger,
	}
	if err := wp.StoreModTime(); err != nil {
		return nil, err
	}
	return wp, nil
}

func (wp *watchPath) IsUpdated() bool {
	wpLatestModTime, err := wp.latestModTime()
	if err != nil {
		wp.log.WithError(err).Errorf("failed to get latest modification time: '%s'", wp.path)
		return false
	}
	wpPreviousModTime := wp.modTime.Load()
	return wpPreviousModTime == nil || !wpLatestModTime.Equal(*wpPreviousModTime)
}

func (wp *watchPath) StoreModTime() error {
	latestModTime, err := wp.latestModTime()
	if err != nil {
		return err
	}
	wp.modTime.Store(&latestModTime)
	return nil
}

func (wp *watchPath) latestModTime() (time.Time, error) {
	f, err := filepath.EvalSymlinks(wp.path)
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to eval file path '%s': '%s'", wp.path, err)
	}
	fi, err := os.Stat(f)
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to get file info '%s': '%s'", f, err)
	}
	return fi.ModTime().UTC(), nil
}
