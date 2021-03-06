// Copyright 2017 Google Inc. All Rights Reserved.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//     http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os"
	"path"
	"strings"
	"time"

	apiv1 "github.com/ericchiang/k8s/api/v1"
	certificates "github.com/ericchiang/k8s/apis/certificates/v1beta1"

	"github.com/ericchiang/k8s"
	"github.com/ericchiang/k8s/apis/meta/v1"
	"github.com/youmark/pkcs8"
)

var (
	additionalDNSNames  string
	certDir             string
	clusterDomain       string
	headlessNameAsCN    bool
	hostname            string
	namespace           string
	pkcs8Format         bool
	podIP               string
	podName             string
	serviceIPs          string
	serviceNames        string
	subdomain           string
	labels              string
	secretName          string
	keysize             int
	countries           string
	organizations       string
	organizationalUnits string
)

func main() {
	flag.StringVar(&additionalDNSNames, "additional-dnsnames", "", "additional dns names; comma separated")
	flag.StringVar(&certDir, "cert-dir", "", "The directory where the TLS certs should be written")
	flag.StringVar(&clusterDomain, "cluster-domain", "cluster.local", "Kubernetes cluster domain")
	flag.BoolVar(&headlessNameAsCN, "headless-name-as-cn", false, "If a headless domain name is provided, use it as CN")
	flag.StringVar(&hostname, "hostname", "", "hostname as defined by pod.spec.hostname")
	flag.StringVar(&namespace, "namespace", "default", "namespace as defined by pod.metadata.namespace")
	flag.BoolVar(&pkcs8Format, "pkcs8", false, "output secret in unencrypted PKCS#8 (java does not support PKCS#1)")
	flag.StringVar(&podName, "pod-name", "", "name as defined by pod.metadata.name")
	flag.StringVar(&podIP, "pod-ip", "", "IP address as defined by pod.status.podIP")
	flag.StringVar(&serviceNames, "service-names", "", "service names that resolve to this Pod; comma separated")
	flag.StringVar(&serviceIPs, "service-ips", "", "service IP addresses that resolve to this Pod; comma separated")
	flag.StringVar(&subdomain, "subdomain", "", "subdomain as defined by pod.spec.subdomain")
	flag.StringVar(&labels, "labels", "", "labels to include in CertificateSigningRequest object; comma seprated list of key=value")
	flag.StringVar(&secretName, "secret-name", "", "secret name to store generated files, will not be persisted to disk")
	flag.IntVar(&keysize, "keysize", 2048, "bit size of private key")
	flag.StringVar(&countries, "countries", "", "The Cs set on the certificate request, comma separated if more than one")
	flag.StringVar(&organizations, "organizations", "", "The Os set on the certificate request, comma separated")
	flag.StringVar(&organizationalUnits, "organizational-units", "", "The OUs set on the certificate request, comma separated")
	flag.Parse()

	certificateSigningRequestName := fmt.Sprintf("%s-%s", podName, namespace)

	client, err := k8s.NewInClusterClient()
	if err != nil {
		log.Fatalf("unable to create a Kubernetes client: %s", err)
	}

	if certDir != "" && secretName != "" {
		log.Fatal("-cert-dir and -secret-name does not make sense together")
	}

	if certDir == "" {
		certDir = "/etc/tls"
	}

	// Before we do anything, if we are storing in a secret, make sure it doesn't contain TLS data already.
	var secret *apiv1.Secret
	if secretName != "" {
		for {
			ks, err := client.CoreV1().GetSecret(context.Background(), secretName, namespace)
			if err != nil {
				log.Printf("Secret to store credentials (%s) not found; trying again in 5 seconds", secretName)
				time.Sleep(5 * time.Second)
				continue
			}
			secretData := ks.GetData()
			for _, file := range [...]string{"tls.key", "tls.crt", "ca.crt"} {
				if _, present := secretData[file]; !present {
					log.Printf("Missing file %s... continuing to generate keys and certificates", file)
					secret = ks
					break
				}
			}
			if secret != nil {
				break
			}
			log.Println("Secret is present and contains data, will exit.")
			os.Exit(0)
		}
	}
	// Generate a private key, pem encode it, and save it to the filesystem.
	// The private key will be used to create a certificate signing request (csr)
	// that will be submitted to a Kubernetes CA to obtain a TLS certificate.
	key, err := rsa.GenerateKey(rand.Reader, keysize)
	if err != nil {
		log.Fatalf("unable to genarate the private key: %s", err)
	}

	var ptype string
	var pkey []byte
	if pkcs8Format {
		ptype = "PRIVATE KEY"
		pkey, err = pkcs8.ConvertPrivateKeyToPKCS8(key)
		if err != nil {
			panic(err)
		}
	} else {
		ptype = "RSA PRIVATE KEY"
		pkey = x509.MarshalPKCS1PrivateKey(key)
	}

	pemKeyBytes := pem.EncodeToMemory(&pem.Block{
		Type:  ptype,
		Bytes: pkey,
	})

	if secretName == "" {
		keyFile := path.Join(certDir, "tls.key")
		if err := ioutil.WriteFile(keyFile, pemKeyBytes, 0644); err != nil {
			log.Fatalf("unable to write to %s: %s", keyFile, err)
		}

		log.Printf("wrote %s", keyFile)
	}

	// Gather the list of labels that will be added to the CreateCertificateSigningRequest object
	labelsMap := make(map[string]string)

	for _, n := range strings.Split(labels, ",") {
		if n == "" {
			continue
		}
		s := strings.Split(n, "=")
		label, key := s[0], s[1]
		if label == "" {
			continue
		}
		labelsMap[label] = key
	}

	// Gather the list of IP addresses for the certificate's IP SANs field which
	// include:
	//   - the pod IP address
	//   - each service IP address that maps to this pod
	ip := net.ParseIP(podIP)
	if ip.To4() == nil && ip.To16() == nil {
		log.Fatal("invalid pod IP address")
	}

	ipaddresses := []net.IP{ip}

	for _, s := range strings.Split(serviceIPs, ",") {
		if s == "" {
			continue
		}
		ip := net.ParseIP(s)
		if ip.To4() == nil && ip.To16() == nil {
			log.Fatal("invalid service IP address")
		}
		ipaddresses = append(ipaddresses, ip)
	}

	// Gather a list of DNS names that resolve to this pod which include the
	// default DNS name:
	//   - ${pod-ip-address}.${namespace}.pod.${cluster-domain}
	//
	// For each service that maps to this pod a dns name will be added using
	// the following template:
	//   - ${service-name}.${namespace}.svc.${cluster-domain}
	//
	// A dns name will be added for each additional DNS name provided via the
	// `-additional-dnsnames` flag.
	dnsNames := defaultDNSNames(podIP, hostname, subdomain, namespace, clusterDomain)

	for _, n := range strings.Split(additionalDNSNames, ",") {
		if n == "" {
			continue
		}
		dnsNames = append(dnsNames, n)
	}

	for _, n := range strings.Split(serviceNames, ",") {
		if n == "" {
			continue
		}
		dnsNames = append(dnsNames, serviceDomainName(n, namespace, clusterDomain))
	}

	// We need to make sure to send in uninitialized values if no value is set, otherwise we get empty fields
	// in the CSR
	var (
		nameCountry            []string
		nameOrganization       []string
		nameOrganizationalUnit []string
	)
	if len(countries) > 0 {
		nameCountry = strings.Split(countries, ",")
	}
	if len(organizations) > 0 {
		nameOrganization = strings.Split(organizations, ",")
	}
	if len(organizationalUnits) > 0 {
		nameOrganizationalUnit = strings.Split(organizationalUnits, ",")
	}
	// Generate the certificate request, pem encode it, and save it to the filesystem.
	certificateRequestTemplate := x509.CertificateRequest{
		Subject: pkix.Name{
			CommonName:         dnsNames[0],
			Country:            nameCountry,
			Organization:       nameOrganization,
			OrganizationalUnit: nameOrganizationalUnit,
		},
		SignatureAlgorithm: x509.SHA256WithRSA,
		DNSNames:           dnsNames,
		IPAddresses:        ipaddresses,
	}

	certificateRequest, err := x509.CreateCertificateRequest(rand.Reader, &certificateRequestTemplate, key)
	if err != nil {
		log.Fatalf("unable to generate the certificate request: %s", err)
	}

	certificateRequestBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: certificateRequest})

	if secretName == "" {
		csrFile := path.Join(certDir, "tls.csr")
		if err := ioutil.WriteFile(csrFile, certificateRequestBytes, 0644); err != nil {
			log.Fatalf("unable to %s, error: %s", csrFile, err)
		}

		log.Printf("wrote %s", csrFile)
	}

	// Submit a certificate signing request, wait for it to be approved, then save
	// the signed certificate to the file system.
	certificateSigningRequest := &certificates.CertificateSigningRequest{
		Metadata: &v1.ObjectMeta{
			Name:   k8s.String(certificateSigningRequestName),
			Labels: labelsMap,
		},
		Spec: &certificates.CertificateSigningRequestSpec{
			Groups:   []string{"system:authenticated"},
			Request:  certificateRequestBytes,
			KeyUsage: []string{"digital signature", "key encipherment", "server auth", "client auth"},
		},
	}

	log.Printf("Deleting certificate signing request  %s", certificateSigningRequestName)
	client.CertificatesV1Beta1().DeleteCertificateSigningRequest(context.Background(), certificateSigningRequestName)
	log.Printf("Removed approved request %s", certificateSigningRequestName)

	_, err = client.CertificatesV1Beta1().GetCertificateSigningRequest(context.Background(), certificateSigningRequestName)
	if err != nil {
		_, err = client.CertificatesV1Beta1().CreateCertificateSigningRequest(context.Background(), certificateSigningRequest)
		if err != nil {
			log.Fatalf("unable to create the certificate signing request: %s", err)
		}
		log.Println("waiting for certificate...")
	} else {
		log.Println("signing request already exists")
	}

	var certificate []byte
	for {
		csr, err := client.CertificatesV1Beta1().GetCertificateSigningRequest(context.Background(), certificateSigningRequestName)
		if err != nil {
			log.Printf("unable to retrieve certificate signing request (%s): %s", certificateSigningRequestName, err)
			time.Sleep(5 * time.Second)
			continue
		}

		if len(csr.GetStatus().GetConditions()) > 0 {
			if *csr.GetStatus().GetConditions()[0].Type == "Approved" {
				certificate = csr.GetStatus().Certificate
				if len(certificate) > 1 {
					log.Printf("got crt %s", certificate)
					break
				} else {
					log.Printf("cert length still less than 1, wait to populate. Cert: %s", csr.GetStatus())
				}

			}
		} else {
			log.Printf("certificate signing request (%s) not approved; trying again in 5 seconds", certificateSigningRequestName)
		}

		time.Sleep(5 * time.Second)
	}

	if secretName == "" {
		certFile := path.Join(certDir, "tls.crt")
		if err := ioutil.WriteFile(certFile, certificate, 0644); err != nil {
			log.Fatalf("unable to write to %s: %s", certFile, err)
		}
		log.Printf("wrote %s", certFile)
	}

	log.Printf("Deleting certificate signing request  %s", certificateSigningRequestName)
	client.CertificatesV1Beta1().DeleteCertificateSigningRequest(context.Background(), certificateSigningRequestName)
	log.Printf("Removed approved request %s", certificateSigningRequestName)

	if secret != nil {
		k8sCrt, err := ioutil.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt")
		if err != nil {
			panic(err)
		}

		stringData := make(map[string]string)
		stringData["tls.key"] = string(pemKeyBytes)
		stringData["tls.crt"] = string(certificate)
		stringData["ca.crt"] = string(k8sCrt) // ok

		secret.StringData = stringData
		_, err = client.CoreV1().UpdateSecret(context.TODO(), secret)
		log.Printf("Stored credentials in secret: (%s)", secretName)
	}

	os.Exit(0)
}

func defaultDNSNames(ip, hostname, subdomain, namespace, clusterDomain string) []string {
	ns := []string{podDomainName(ip, namespace, clusterDomain)}
	if hostname != "" && subdomain != "" {
		headlessName := podHeadlessDomainName(hostname, subdomain, namespace, clusterDomain)
		if headlessNameAsCN {
			ns = append([]string{headlessName}, ns...)
		} else {
			ns = append(ns, headlessName)
		}
	}
	return ns
}

func serviceDomainName(name, namespace, domain string) string {
	return fmt.Sprintf("%s.%s.svc.%s", name, namespace, domain)
}

func podDomainName(ip, namespace, domain string) string {
	return fmt.Sprintf("%s.%s.pod.%s", strings.Replace(ip, ".", "-", -1), namespace, domain)
}

func podHeadlessDomainName(hostname, subdomain, namespace, domain string) string {
	if hostname == "" || subdomain == "" {
		return ""
	}
	return fmt.Sprintf("%s.%s.%s.svc.%s", hostname, subdomain, namespace, domain)
}
