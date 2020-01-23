/*
Copyright © 2019 Alessandro Segala (@ItalyPaleAle)

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published
by the Free Software Foundation, version 3 of the License.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program.  If not, see <https://www.gnu.org/licenses/>.
*/

package sync

import (
	"time"

	"smplatform/appmanager"
	"smplatform/state"
	"smplatform/webserver"
)

// Semaphore that allows only one operation at time
var semaphore = make(chan int, 1)

// Last time the sync was started
var lastSync *time.Time

// Last sync error
var syncError error

// QueueRun is a thread-safe version of Run that ensures that only one sync can happen at a time
func QueueRun() {
	semaphore <- 1
	syncError = nil
	go func() {
		syncError = runner()
		if syncError != nil {
			logger.Println("[syncState] Error returned by async run", syncError)
		}
		<-semaphore
	}()
}

// Run ensures the system is in the correct state
// You should use QueueRun in most cases
func Run() error {
	semaphore <- 1
	syncError = runner()
	<-semaphore
	return syncError
}

// IsRunning returns true if the sync is running in background
func IsRunning() bool {
	return len(semaphore) == 1
}

// LastSync returns the time when the last sync started
func LastSync() *time.Time {
	return lastSync
}

// SyncError returns the error (if any) during the last sync
func SyncError() error {
	return syncError
}

// Function actually executing the sync
func runner() error {
	// Set the time
	now := time.Now()
	lastSync = &now

	// Boolean flag for the need to restart the webserver
	restartRequired := false

	// Get the list of sites
	sites := state.Instance.GetSites()

	// First, sync apps
	res, err := appmanager.Instance.SyncState(sites)
	if err != nil {
		logger.Println("[syncState] Unrecoverable error while syncing apps:", err)
		return err
	}
	restartRequired = restartRequired || res

	// Second, sync the web server configuration
	res, err = webserver.Instance.SyncConfiguration(sites)
	if err != nil {
		logger.Println("[syncState] Error while syncing Nginx configuration:", err)
		return err
	}
	restartRequired = restartRequired || res

	// If we've updated anything that requires restarting nginx, do it
	if restartRequired {
		if err := webserver.Instance.RestartServer(); err != nil {
			logger.Println("[syncState] Error while restarting Nginx:", err)
			return err
		}

		// Sleep for 0.15 seconds waiting for the server to restart
		time.Sleep(150 * time.Millisecond)
	}

	return nil
}
