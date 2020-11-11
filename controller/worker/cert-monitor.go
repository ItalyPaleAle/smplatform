/*
Copyright © 2020 Alessandro Segala (@ItalyPaleAle)

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

package worker

import (
	"context"
	"crypto/x509"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"

	"github.com/statiko-dev/statiko/buildinfo"
	"github.com/statiko-dev/statiko/controller/certificates"
	"github.com/statiko-dev/statiko/shared/certutils"
	pb "github.com/statiko-dev/statiko/shared/proto"
)

// Init the cert monitor worker
func (w *Worker) initCertMonitorWorker() {
	// Logger for this worker
	w.certMonitorLogger = log.New(buildinfo.LogDestination, "worker/cert-monitor: ", log.Ldate|log.Ltime|log.LUTC)

	// Notifications sent
	w.certMonitorNotifications = make(map[string]int)

	// Send notifications when the certificate is expiring in N days
	w.certMonitorChecks = []int{-2, -1, 0, 1, 2, 3, 7, 14, 30}

	// Set the callback to refresh the certs
	w.certMonitorRefreshCh = make(chan int)
	w.State.CertRefresh = func() {
		w.certMonitorRefreshCh <- 1
	}
}

// In background, periodically check for expired certificates
func (w *Worker) startCertMonitorWorker(ctx context.Context) {
	// Set variables
	certMonitorInterval := time.Duration(24 * time.Hour) // Run every 24 hours

	go func() {
		// Run on ticker
		ticker := time.NewTicker(certMonitorInterval)
		defer ticker.Stop()

		// Do not run right away
		// The controller will call TriggerCertRefresh when the first node joins
		//w.State.TriggerCertRefresh()

		for {
			select {
			case <-ticker.C:
				err := w.certMonitorWorker()
				if err != nil {
					w.certMonitorLogger.Println("Worker error:", err)
				}
			case <-w.certMonitorRefreshCh:
				err := w.certMonitorWorker()
				if err != nil {
					w.certMonitorLogger.Println("Worker error:", err)
				}
			case <-ctx.Done():
				w.certMonitorLogger.Println("Worker's context canceled")
				return
			}
		}
	}()
}

// Look up all certificates to look for those expiring
func (w *Worker) certMonitorWorker() error {
	w.certMonitorLogger.Println("Starting cert-monitor worker")

	// Go through all sites
	sites := w.State.GetSites()
	for _, el := range sites {
		// List of domains
		domains := append([]string{el.Domain}, el.Aliases...)

		// Check if there's a generated TLS certificate for this site
		if el.GeneratedTlsId != "" {
			// If we want to use ACME, take this path
			if el.EnableAcme {
				// Errors are already logged
				_ = w.certMonitorInspectACME(el.GeneratedTlsId, domains)
			} else {
				// Errors are already logged
				_ = w.certMonitorInspectSelfSigned(el.GeneratedTlsId, domains)
			}
		}

		// Check if there's an imported TLS certificate for this site
		if el.ImportedTlsId != "" {
			// Errors are already logged
			_ = w.certMonitorInspectImported(el.ImportedTlsId, domains)
		}
	}

	w.certMonitorLogger.Println("Done")

	return nil
}

// Loads a certificate and returns its x509.Certificate object
func (w *Worker) certMonitorLoadCert(certId string) (certX509 *x509.Certificate, err error) {
	// Load the certificate and parse the PEM
	_, certPem, err := w.Certificates.GetCertificate(certId)
	if err != nil || len(certPem) == 0 {
		if err == certutils.NotFoundErr || len(certPem) == 0 {
			w.certMonitorLogger.Printf("Certificate %s not found\n", certId)
		} else {
			w.certMonitorLogger.Printf("Error while obtaining certificate %s: %s\n", certId, err)
		}
		return nil, err
	}
	cert, err := certificates.GetX509(certPem)
	if err != nil {
		w.certMonitorLogger.Printf("Could not parse PEM data for certificate %s: %s", certId, err)
		return nil, err
	}

	return cert, nil
}

// Checks whether a generated certificate is expired and whether it's self-signed
func (w *Worker) certMonitorCheck(certId string, days int) (expired bool, selfSigned bool, err error) {
	now := time.Now()

	// Load the certificate
	cert, err := w.certMonitorLoadCert(certId)
	if err != nil {
		// Error was already logged
		return false, false, err
	}

	// Check if the certificate is self-signed
	selfSigned = len(cert.Issuer.Organization) > 0 &&
		cert.Issuer.Organization[0] == certificates.SelfSignedCertificateIssuer

	// Interval before we need to request new certs
	// ACME and self-signed certs have a different time before we need to update them
	expired = cert.NotAfter.Before(now.Add(time.Duration((days * 24)) * time.Hour))
	return expired, selfSigned, nil
}

// Stores a new TLS certificate in the state (but does not run the replace method)
func (w *Worker) certMonitorStoreGenerated(keyPem []byte, certPem []byte, certObj *pb.TLSCertificate, certId string, domain string) (rr error) {
	// Get the X509 object
	certX509, err := certificates.GetX509(certPem)
	if err != nil {
		w.certMonitorLogger.Printf("Could not parse PEM data for the new certificate for site %s: %s", domain, err)
		return err
	}
	certObj.SetCertificateProperties(certX509)

	// Set the certificate
	err = w.State.SetCertificate(certObj, certId, keyPem, certPem)
	if err != nil {
		w.certMonitorLogger.Printf("Could not store the new certificate for site %s: %s", domain, err)
		return err
	}

	return nil
}

// Inspect a self-signed certificate, and if it's about to expire, re-generate it
func (w *Worker) certMonitorInspectSelfSigned(certId string, domains []string) error {
	// Check if the certificate has expired or if it's self-signed
	expired, _, err := w.certMonitorCheck(certId, certificates.SelfSignedMinDays)
	if err != nil {
		// Error was already logged
		return err
	}

	// If the self-signed certificate has expired, re-generate it
	if expired {
		w.certMonitorLogger.Printf("Certificate for site %s is expiring in less than %d days; regenerating it\n", domains[0], certificates.SelfSignedMinDays)

		// Get a new self-signed cert
		keyPem, certPem, err := certificates.GenerateTLSCert(domains...)
		if err != nil {
			w.certMonitorLogger.Printf("Error while generating a new certificate for site %s: %s\n", domains[0], err)
			return err
		}

		// Generate a new ID for the certificate
		u, err := uuid.NewRandom()
		if err != nil {
			w.certMonitorLogger.Println("Error while generating a UUID:", err)
			return err
		}
		newCertId := u.String()

		// Store the certificate
		certObj := &pb.TLSCertificate{
			Type: pb.TLSCertificate_SELF_SIGNED,
		}
		err = w.certMonitorStoreGenerated(keyPem, certPem, certObj, newCertId, domains[0])
		if err != nil {
			// Error was already logged
			return err
		}

		// Replace the certificate for all sites using it
		err = w.State.ReplaceCertificate(certId, newCertId)
		if err != nil {
			w.certMonitorLogger.Printf("Could not replace the certificate for site %s: %s", domains[0], err)
			return err
		}
	}

	return nil
}

// Inspect a certificate from ACME, and if it's about to expire, request a new one
// If the current certificate is self-signed (and not from ACME), always request a new one
func (w *Worker) certMonitorInspectACME(certId string, domains []string) error {
	// Check if the certificate has expired or if it's self-signed
	expired, selfSigned, err := w.certMonitorCheck(certId, certificates.ACMEMinDays)
	if err != nil {
		// Error was already logged
		return err
	}

	// If the certificate has expired, or if it's self-signed but we want to use ACME, request a new certificate
	if expired || selfSigned {
		w.certMonitorLogger.Printf("Requesting a new certificate for site %s from ACME\n", domains[0])

		// Get the certificate from ACME (this can be a blocking call)
		keyPem, certPem, err := w.Certificates.GenerateACMECertificate(domains...)
		if err != nil {
			w.certMonitorLogger.Printf("Error while requesting certificate from ACME for site %s: %s\n", domains[0], err)
			return err
		}

		// Generate a new ID for the certificate
		u, err := uuid.NewRandom()
		if err != nil {
			w.certMonitorLogger.Println("Error while generating a UUID:", err)
			return err
		}
		newCertId := u.String()

		// Store the certificate
		certObj := &pb.TLSCertificate{
			Type: pb.TLSCertificate_ACME,
		}
		err = w.certMonitorStoreGenerated(keyPem, certPem, certObj, newCertId, domains[0])
		if err != nil {
			// Error was already logged
			return err
		}

		// Replace the certificate for all sites using it
		err = w.State.ReplaceCertificate(certId, newCertId)
		if err != nil {
			w.certMonitorLogger.Printf("Could not replace the certificate for site %s: %s", domains[0], err)
			return err
		}
	}

	return nil
}

// For imported certificates, send a notification if the cert has expired
func (w *Worker) certMonitorInspectImported(certId string, domains []string) error {
	now := time.Now()

	// Load the certificate
	cert, err := w.certMonitorLoadCert(certId)
	if err != nil {
		// Error was already logged
		return err
	}

	// Check if we sent a notification for expiring certificates already
	sent, found := w.certMonitorNotifications[domains[0]]
	if !found {
		sent = len(w.certMonitorChecks)
	}

	// Go through all checks
	for i := 0; i < len(w.certMonitorChecks); i++ {
		// If the certificate has expired
		// Note: we are assuming 24-hour days, which isn't always correct but it's fine in this case
		expired := cert.NotAfter.Before(now.Add(time.Duration((w.certMonitorChecks[i] * 24)) * time.Hour))
		if expired {
			// If we haven't already sent this notification
			if i < sent {
				message := "Certificate for " + domains[0] + " "
				switch w.certMonitorChecks[i] {
				case -2:
					message += "has expired over 2 days ago"
				case -1:
					message += "has expired 1 day ago"
				case 0:
					message += "has expired today"
				case 1:
					message += "is expiring today"
				default:
					message += fmt.Sprintf("expires in %d days", w.certMonitorChecks[i])
				}
				w.certMonitorNotifications[domains[0]] = i

				// Send the notification in background
				go w.Notifier.SendNotification(message)
				break
			}
		}
	}

	return nil
}
