// Copyright 2020 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package tidbcluster

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"text/template"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/pingcap/tidb-operator/pkg/controller"
	"github.com/pingcap/tidb-operator/pkg/util"
	"github.com/pingcap/tidb-operator/tests/e2e/util/portforward"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	"k8s.io/kubernetes/test/e2e/framework/pod"
)

var tidbIssuerTmpl = `
apiVersion: cert-manager.io/v1alpha2
kind: Issuer
metadata:
  name: {{ .ClusterName }}-selfsigned-ca-issuer
  namespace: {{ .Namespace }}
spec:
  selfSigned: {}
---
apiVersion: cert-manager.io/v1alpha2
kind: Certificate
metadata:
  name: {{ .ClusterName }}-ca
  namespace: {{ .Namespace }}
spec:
  secretName: {{ .ClusterName }}-ca-secret
  commonName: "TiDB CA"
  isCA: true
  issuerRef:
    name: {{ .ClusterRef }}-selfsigned-ca-issuer
    kind: Issuer
---
apiVersion: cert-manager.io/v1alpha2
kind: Issuer
metadata:
  name: {{ .ClusterName }}-tidb-issuer
  namespace: {{ .Namespace }}
spec:
  ca:
    secretName: {{ .ClusterName }}-ca-secret
`

var tidbCertificatesTmpl = `
apiVersion: cert-manager.io/v1alpha2
kind: Certificate
metadata:
  name: {{ .ClusterName }}-tidb-server-secret
  namespace: {{ .Namespace }}
spec:
  secretName: {{ .ClusterName }}-tidb-server-secret
  duration: 8760h # 365d
  renewBefore: 360h # 15d
  organization:
    - PingCAP
  commonName: "TiDB Server"
  usages:
    - server auth
  dnsNames:
    - "{{ .ClusterName }}-tidb"
    - "{{ .ClusterName }}-tidb.{{ .Namespace }}"
    - "*.{{ .ClusterName }}-tidb"
    - "{{ .ClusterName }}-tidb.{{ .Namespace }}.svc{{ .ClusterDomain }}"
    - "*.{{ .ClusterName }}-tidb.{{ .Namespace }}"
    - "*.{{ .ClusterName }}-tidb.{{ .Namespace }}.svc{{ .ClusterDomain }}"
  ipAddresses:
    - 127.0.0.1
    - ::1
  issuerRef:
    name: {{ .ClusterRef }}-tidb-issuer
    kind: Issuer
    group: cert-manager.io
---
apiVersion: cert-manager.io/v1alpha2
kind: Certificate
metadata:
  name: {{ .ClusterName }}-tidb-client-secret
  namespace: {{ .Namespace }}
spec:
  secretName: {{ .ClusterName }}-tidb-client-secret
  duration: 8760h # 365d
  renewBefore: 360h # 15d
  organization:
    - PingCAP
  commonName: "TiDB Client"
  usages:
    - client auth
  issuerRef:
    name: {{ .ClusterRef }}-tidb-issuer
    kind: Issuer
    group: cert-manager.io
`

var tidbComponentsOnlyPDCertificatesTmpl = `
apiVersion: cert-manager.io/v1alpha2
kind: Certificate
metadata:
  name: {{ .ClusterName }}-pd-cluster-secret
  namespace: {{ .Namespace }}
spec:
  secretName: {{ .ClusterName }}-pd-cluster-secret
  duration: 8760h # 365d
  renewBefore: 360h # 15d
  organization:
  - PingCAP
  commonName: "TiDB"
  usages:
    - server auth
    - client auth
  dnsNames:
  - "{{ .ClusterName }}-pd"
  - "{{ .ClusterName }}-pd.{{ .Namespace }}"
  - "{{ .ClusterName }}-pd.{{ .Namespace }}.svc{{ .ClusterDomain }}"
  - "{{ .ClusterName }}-pd-peer"
  - "{{ .ClusterName }}-pd-peer.{{ .Namespace }}"
  - "{{ .ClusterName }}-pd-peer.{{ .Namespace }}.svc{{ .ClusterDomain }}"
  - "*.{{ .ClusterName }}-pd-peer"
  - "*.{{ .ClusterName }}-pd-peer.{{ .Namespace }}"
  - "*.{{ .ClusterName }}-pd-peer.{{ .Namespace }}.svc{{ .ClusterDomain }}"
  ipAddresses:
  - 127.0.0.1
  - ::1
  issuerRef:
    name: {{ .ClusterRef }}-tidb-issuer
    kind: Issuer
    group: cert-manager.io
`

var tidbComponentsExceptPDCertificatesTmpl = `
apiVersion: cert-manager.io/v1alpha2
kind: Certificate
metadata:
  name: {{ .ClusterName }}-tikv-cluster-secret
  namespace: {{ .Namespace }}
spec:
  secretName: {{ .ClusterName }}-tikv-cluster-secret
  duration: 8760h # 365d
  renewBefore: 360h # 15d
  organization:
  - PingCAP
  commonName: "TiDB"
  usages:
    - server auth
    - client auth
  dnsNames:
  - "{{ .ClusterName }}-tikv"
  - "{{ .ClusterName }}-tikv.{{ .Namespace }}"
  - "{{ .ClusterName }}-tikv.{{ .Namespace }}.svc{{ .ClusterDomain }}"
  - "{{ .ClusterName }}-tikv-peer"
  - "{{ .ClusterName }}-tikv-peer.{{ .Namespace }}"
  - "{{ .ClusterName }}-tikv-peer.{{ .Namespace }}.svc{{ .ClusterDomain }}"
  - "*.{{ .ClusterName }}-tikv-peer"
  - "*.{{ .ClusterName }}-tikv-peer.{{ .Namespace }}"
  - "*.{{ .ClusterName }}-tikv-peer.{{ .Namespace }}.svc{{ .ClusterDomain }}"
  ipAddresses:
  - 127.0.0.1
  - ::1
  issuerRef:
    name: {{ .ClusterRef }}-tidb-issuer
    kind: Issuer
    group: cert-manager.io
---
apiVersion: cert-manager.io/v1alpha2
kind: Certificate
metadata:
  name: {{ .ClusterName }}-tidb-cluster-secret
  namespace: {{ .Namespace }}
spec:
  secretName: {{ .ClusterName }}-tidb-cluster-secret
  duration: 8760h # 365d
  renewBefore: 360h # 15d
  organization:
  - PingCAP
  commonName: "TiDB"
  usages:
    - server auth
    - client auth
  dnsNames:
  - "{{ .ClusterName }}-tidb"
  - "{{ .ClusterName }}-tidb.{{ .Namespace }}"
  - "{{ .ClusterName }}-tidb.{{ .Namespace }}.svc{{ .ClusterDomain }}"
  - "{{ .ClusterName }}-tidb-peer"
  - "{{ .ClusterName }}-tidb-peer.{{ .Namespace }}"
  - "{{ .ClusterName }}-tidb-peer.{{ .Namespace }}.svc{{ .ClusterDomain }}"
  - "*.{{ .ClusterName }}-tidb-peer"
  - "*.{{ .ClusterName }}-tidb-peer.{{ .Namespace }}"
  - "*.{{ .ClusterName }}-tidb-peer.{{ .Namespace }}.svc{{ .ClusterDomain }}"
  ipAddresses:
  - 127.0.0.1
  - ::1
  issuerRef:
    name: {{ .ClusterRef }}-tidb-issuer
    kind: Issuer
    group: cert-manager.io
---
apiVersion: cert-manager.io/v1alpha2
kind: Certificate
metadata:
  name: {{ .ClusterName }}-cluster-client-secret
  namespace: {{ .Namespace }}
spec:
  secretName: {{ .ClusterName }}-cluster-client-secret
  duration: 8760h # 365d
  renewBefore: 360h # 15d
  organization:
  - PingCAP
  commonName: "TiDB"
  usages:
    - client auth
  issuerRef:
    name: {{ .ClusterRef }}-tidb-issuer
    kind: Issuer
    group: cert-manager.io
---
apiVersion: cert-manager.io/v1alpha2
kind: Certificate
metadata:
  name: {{ .ClusterName }}-pump-cluster-secret
  namespace: {{ .Namespace }}
spec:
  secretName: {{ .ClusterName }}-pump-cluster-secret
  duration: 8760h # 365d
  renewBefore: 360h # 15d
  organization:
  - PingCAP
  commonName: "TiDB"
  usages:
    - server auth
    - client auth
  dnsNames:
  - "*.{{ .ClusterName }}-pump"
  - "*.{{ .ClusterName }}-pump.{{ .Namespace }}"
  - "*.{{ .ClusterName }}-pump.{{ .Namespace }}.svc{{ .ClusterDomain }}"
  ipAddresses:
  - 127.0.0.1
  - ::1
  issuerRef:
    name: {{ .ClusterRef }}-tidb-issuer
    kind: Issuer
    group: cert-manager.io
---
apiVersion: cert-manager.io/v1alpha2
kind: Certificate
metadata:
  name: {{ .ClusterName }}-drainer-cluster-secret
  namespace: {{ .Namespace }}
spec:
  secretName: {{ .ClusterName }}-drainer-cluster-secret
  duration: 8760h # 365d
  renewBefore: 360h # 15d
  organization:
  - PingCAP
  commonName: "TiDB"
  usages:
    - server auth
    - client auth
  dnsNames:
  - "*.{{ .ClusterName }}-{{ .ClusterName }}-drainer"
  - "*.{{ .ClusterName }}-{{ .ClusterName }}-drainer.{{ .Namespace }}"
  - "*.{{ .ClusterName }}-{{ .ClusterName }}-drainer.{{ .Namespace }}.svc{{ .ClusterDomain }}"
  ipAddresses:
  - 127.0.0.1
  - ::1
  issuerRef:
    name: {{ .ClusterRef }}-tidb-issuer
    kind: Issuer
    group: cert-manager.io
---
apiVersion: cert-manager.io/v1alpha2
kind: Certificate
metadata:
  name: {{ .ClusterName }}-tiflash-cluster-secret
  namespace: {{ .Namespace }}
spec:
  secretName: {{ .ClusterName }}-tiflash-cluster-secret
  duration: 8760h # 365d
  renewBefore: 360h # 15d
  organization:
  - PingCAP
  commonName: "TiDB"
  usages:
    - server auth
    - client auth
  dnsNames:
  - "{{ .ClusterName }}-tiflash"
  - "{{ .ClusterName }}-tiflash.{{ .Namespace }}"
  - "{{ .ClusterName }}-tiflash.{{ .Namespace }}.svc{{ .ClusterDomain }}"
  - "{{ .ClusterName }}-tiflash-peer"
  - "{{ .ClusterName }}-tiflash-peer.{{ .Namespace }}"
  - "{{ .ClusterName }}-tiflash-peer.{{ .Namespace }}.svc{{ .ClusterDomain }}"
  - "*.{{ .ClusterName }}-tiflash-peer"
  - "*.{{ .ClusterName }}-tiflash-peer.{{ .Namespace }}"
  - "*.{{ .ClusterName }}-tiflash-peer.{{ .Namespace }}.svc{{ .ClusterDomain }}"
  ipAddresses:
  - 127.0.0.1
  - ::1
  issuerRef:
    name: {{ .ClusterRef }}-tidb-issuer
    kind: Issuer
    group: cert-manager.io
---
apiVersion: cert-manager.io/v1alpha2
kind: Certificate
metadata:
  name: {{ .ClusterName }}-ticdc-cluster-secret
  namespace: {{ .Namespace }}
spec:
  secretName: {{ .ClusterName }}-ticdc-cluster-secret
  duration: 8760h # 365d
  renewBefore: 360h # 15d
  organization:
  - PingCAP
  commonName: "TiDB"
  usages:
  - server auth
  - client auth
  dnsNames:
  - "{{ .ClusterName }}-ticdc"
  - "{{ .ClusterName }}-ticdc.{{ .Namespace }}"
  - "{{ .ClusterName }}-ticdc.{{ .Namespace }}.svc{{ .ClusterDomain }}"
  - "{{ .ClusterName }}-ticdc-peer"
  - "{{ .ClusterName }}-ticdc-peer.{{ .Namespace }}"
  - "{{ .ClusterName }}-ticdc-peer.{{ .Namespace }}.svc{{ .ClusterDomain }}"
  - "*.{{ .ClusterName }}-ticdc-peer"
  - "*.{{ .ClusterName }}-ticdc-peer.{{ .Namespace }}"
  - "*.{{ .ClusterName }}-ticdc-peer.{{ .Namespace }}.svc{{ .ClusterDomain }}"
  ipAddresses:
  - 127.0.0.1
  - ::1
  issuerRef:
    name: {{ .ClusterRef }}-tidb-issuer
    kind: Issuer
    group: cert-manager.io
`

var tidbClientCertificateTmpl = `
apiVersion: cert-manager.io/v1alpha2
kind: Certificate
metadata:
  name: {{ .ClusterName }}-{{ .Component }}-tls
  namespace: {{ .Namespace }}
spec:
  secretName: {{ .ClusterName }}-{{ .Component }}-tls
  duration: 8760h # 365d
  renewBefore: 360h # 15d
  organization:
    - PingCAP
  commonName: "TiDB Client"
  usages:
    - client auth
  issuerRef:
    name: {{ .ClusterRef }}-tidb-issuer
    kind: Issuer
    group: cert-manager.io
`

var mysqlCertificatesTmpl = `
apiVersion: cert-manager.io/v1alpha2
kind: Certificate
metadata:
  name: {{ .ClusterName }}-mysql-secret
  namespace: {{ .Namespace }}
spec:
  secretName: {{ .ClusterName }}-mysql-secret
  duration: 8760h # 365d
  renewBefore: 360h # 15d
  organization:
    - PingCAP
  commonName: "MySQL Server"
  usages:
    - server auth
    - client auth
  dnsNames:
  - "*.dm-mysql"
  ipAddresses:
  - 127.0.0.1
  - ::1
  issuerRef:
    name: {{ .ClusterRef }}-tidb-issuer  # use tidb-issuer in E2E tests
    kind: Issuer
    group: cert-manager.io
`

var dmCertificatesTmp = `
apiVersion: cert-manager.io/v1alpha2
kind: Certificate
metadata:
  name: {{ .ClusterName }}-dm-master-cluster-secret
  namespace: {{ .Namespace }}
spec:
  secretName: {{ .ClusterName }}-dm-master-cluster-secret
  duration: 8760h # 365d
  renewBefore: 360h # 15d
  organization:
  - PingCAP
  commonName: "TiDB"
  usages:
    - server auth
    - client auth
  dnsNames:
  - "{{ .ClusterName }}-dm-master"
  - "{{ .ClusterName }}-dm-master.{{ .Namespace }}"
  - "{{ .ClusterName }}-dm-master.{{ .Namespace }}.svc"
  - "{{ .ClusterName }}-dm-master-peer"
  - "{{ .ClusterName }}-dm-master-peer.{{ .Namespace }}"
  - "{{ .ClusterName }}-dm-master-peer.{{ .Namespace }}.svc"
  - "*.{{ .ClusterName }}-dm-master-peer"
  - "*.{{ .ClusterName }}-dm-master-peer.{{ .Namespace }}"
  - "*.{{ .ClusterName }}-dm-master-peer.{{ .Namespace }}.svc"
  ipAddresses:
  - 127.0.0.1
  - ::1
  issuerRef:
    name: {{ .ClusterRef }}-tidb-issuer # use tidb-issuer in E2E tests
    kind: Issuer
    group: cert-manager.io
---
apiVersion: cert-manager.io/v1alpha2
kind: Certificate
metadata:
  name: {{ .ClusterName }}-dm-worker-cluster-secret
  namespace: {{ .Namespace }}
spec:
  secretName: {{ .ClusterName }}-dm-worker-cluster-secret
  duration: 8760h # 365d
  renewBefore: 360h # 15d
  organization:
  - PingCAP
  commonName: "TiDB"
  usages:
    - server auth
    - client auth
  dnsNames:
  - "{{ .ClusterName }}-dm-worker"
  - "{{ .ClusterName }}-dm-worker.{{ .Namespace }}"
  - "{{ .ClusterName }}-dm-worker.{{ .Namespace }}.svc"
  - "{{ .ClusterName }}-dm-worker-peer"
  - "{{ .ClusterName }}-dm-worker-peer.{{ .Namespace }}"
  - "{{ .ClusterName }}-dm-worker-peer.{{ .Namespace }}.svc"
  - "*.{{ .ClusterName }}-dm-worker-peer"
  - "*.{{ .ClusterName }}-dm-worker-peer.{{ .Namespace }}"
  - "*.{{ .ClusterName }}-dm-worker-peer.{{ .Namespace }}.svc"
  ipAddresses:
  - 127.0.0.1
  - ::1
  issuerRef:
    name: {{ .ClusterRef }}-tidb-issuer # use tidb-issuer in E2E tests
    kind: Issuer
    group: cert-manager.io
---
apiVersion: cert-manager.io/v1alpha2
kind: Certificate
metadata:
  name: {{ .ClusterName }}-dm-client-secret
  namespace: {{ .Namespace }}
spec:
  secretName: {{ .ClusterName }}-dm-client-secret
  duration: 8760h # 365d
  renewBefore: 360h # 15d
  organization:
  - PingCAP
  commonName: "TiDB"
  usages:
    - client auth
  issuerRef:
    name: {{ .ClusterRef }}-tidb-issuer # use tidb-issuer in E2E tests
    kind: Issuer
    group: cert-manager.io
`

var xK8sTidbIssuerTmpl = `
apiVersion: cert-manager.io/v1alpha2
kind: Issuer
metadata:
  name: {{ .ClusterName }}-tidb-issuer
  namespace: {{ .Namespace }}
spec:
  ca:
    secretName: {{ .ClusterRef }}-ca-secret
`

type tcTmplMeta struct {
	Namespace   string
	ClusterName string
	ClusterRef  string
}

type tcCliTmplMeta struct {
	tcTmplMeta
	Component string
}

type tcCertTmplMeta struct {
	tcTmplMeta
	ClusterDomain string
}

func InstallCertManager(cli clientset.Interface) error {
	cmd := "kubectl apply -f /cert-manager.yaml --validate=false"
	if data, err := exec.Command("sh", "-c", cmd).CombinedOutput(); err != nil {
		return fmt.Errorf("failed to install cert-manager %s %v", string(data), err)
	}

	err := pod.WaitForPodsRunningReady(cli, "cert-manager", 3, 0, 10*time.Minute, nil)
	if err != nil {
		return err
	}

	// It may take a minute or so for the TLS assets required for the webhook to function to be provisioned.
	time.Sleep(time.Minute)
	return nil
}

func DeleteCertManager(cli clientset.Interface) error {
	cmd := "kubectl delete -f /cert-manager.yaml --ignore-not-found"
	if data, err := exec.Command("sh", "-c", cmd).CombinedOutput(); err != nil {
		return fmt.Errorf("failed to delete cert-manager %s %v", string(data), err)
	}

	return wait.PollImmediate(5*time.Second, 10*time.Minute, func() (bool, error) {
		podList, err := cli.CoreV1().Pods("cert-manager").List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			return false, nil
		}
		for _, _pod := range podList.Items {
			err := pod.WaitForPodNotFoundInNamespace(cli, _pod.Name, "cert-manager", 5*time.Minute)
			if err != nil {
				framework.Logf("failed to wait for pod cert-manager/%s disappear", _pod.Name)
				return false, nil
			}
		}

		return true, nil
	})
}

func InstallTiDBIssuer(ns, tcName string) error {
	return installCert(tidbIssuerTmpl, tcTmplMeta{ns, tcName, tcName})
}

func InstallXK8sTiDBIssuer(ns, tcName, clusterRef string) error {
	return installCert(xK8sTidbIssuerTmpl, tcTmplMeta{ns, tcName, clusterRef})
}

func InstallTiDBCertificates(ns, tcName string) error {
	return installCert(tidbCertificatesTmpl, tcCertTmplMeta{tcTmplMeta{ns, tcName, tcName}, ""})
}

func installHeterogeneousTiDBCertificates(ns, tcName string, clusterRef string) error {
	return installCert(tidbCertificatesTmpl, tcCertTmplMeta{tcTmplMeta{ns, tcName, clusterRef}, ""})
}

func InstallXK8sTiDBCertificates(ns, tcName, clusterDomain string) error {
	return installCert(tidbCertificatesTmpl, tcCertTmplMeta{tcTmplMeta{ns, tcName, tcName}, "." + clusterDomain})
}

func InstallTiDBComponentsCertificates(ns, tcName string) error {
	err := installCert(tidbComponentsOnlyPDCertificatesTmpl, tcCertTmplMeta{tcTmplMeta{ns, tcName, tcName}, ""})
	if err != nil {
		return err
	}
	return installCert(tidbComponentsExceptPDCertificatesTmpl, tcCertTmplMeta{tcTmplMeta{ns, tcName, tcName}, ""})
}

func installHeterogeneousTiDBComponentsCertificates(ns, tcName string, clusterRef string) error {
	err := installCert(tidbComponentsOnlyPDCertificatesTmpl, tcCertTmplMeta{tcTmplMeta{ns, tcName, clusterRef}, ""})
	if err != nil {
		return err
	}
	return installCert(tidbComponentsExceptPDCertificatesTmpl, tcCertTmplMeta{tcTmplMeta{ns, tcName, clusterRef}, ""})
}

func InstallXK8sTiDBComponentsCertificates(ns, tcName, clusterDomain string, exceptPD bool) error {
	if !exceptPD {
		err := installCert(tidbComponentsOnlyPDCertificatesTmpl, tcCertTmplMeta{tcTmplMeta{ns, tcName, tcName}, "." + clusterDomain})
		if err != nil {
			return err
		}
	}
	return installCert(tidbComponentsExceptPDCertificatesTmpl, tcCertTmplMeta{tcTmplMeta{ns, tcName, tcName}, "." + clusterDomain})
}

func installTiDBInitializerCertificates(ns, tcName string) error {
	return installCert(tidbClientCertificateTmpl, tcCliTmplMeta{tcTmplMeta{ns, tcName, tcName}, "initializer"})
}

func installPDDashboardCertificates(ns, tcName string) error {
	return installCert(tidbClientCertificateTmpl, tcCliTmplMeta{tcTmplMeta{ns, tcName, tcName}, "dashboard"})
}

func InstallMySQLCertificates(ns, dcName string) error {
	return installCert(mysqlCertificatesTmpl, tcTmplMeta{ns, dcName, dcName})
}

func InstallDMCertificates(ns, dcName string) error {
	return installCert(dmCertificatesTmp, tcTmplMeta{ns, dcName, dcName})
}

func installCert(tmplStr string, tp interface{}) error {
	var buf bytes.Buffer
	tmpl, err := template.New("template").Parse(tmplStr)
	if err != nil {
		return fmt.Errorf("error when parsing template: %v", err)
	}
	err = tmpl.Execute(&buf, tp)
	if err != nil {
		return fmt.Errorf("error when executing template: %v", err)
	}

	tmpFile, err := ioutil.TempFile(os.TempDir(), "tls-")
	if err != nil {
		return err
	}
	_, err = tmpFile.Write(buf.Bytes())
	if err != nil {
		return err
	}
	if data, err := exec.Command("sh", "-c", fmt.Sprintf("kubectl apply -f %s", tmpFile.Name())).CombinedOutput(); err != nil {
		framework.Logf("failed to create certificate: %s, %v", string(data), err)
		return err
	}

	return nil
}

func tidbIsTLSEnabled(fw portforward.PortForward, c clientset.Interface, ns, tcName, passwd string) wait.ConditionFunc {
	return func() (bool, error) {
		db, cancel, err := connectToTiDBWithTLSSupport(fw, c, ns, tcName, passwd, true)
		if err != nil {
			return false, nil
		}
		defer db.Close()
		defer cancel()

		rows, err := db.Query("SHOW STATUS")
		if err != nil {
			return false, err
		}
		var name, value string
		for rows.Next() {
			err := rows.Scan(&name, &value)
			if err != nil {
				return false, err
			}

			if name == "Ssl_cipher" {
				if value == "" {
					return true, fmt.Errorf("the connection to tidb server is not ssl %s/%s", ns, tcName)
				}

				framework.Logf("The connection to TiDB Server is TLS enabled.")
				return true, nil
			}
		}

		return true, fmt.Errorf("can't find Ssl_cipher in status %s/%s", ns, tcName)
	}
}

func insertIntoDataToSourceDB(fw portforward.PortForward, c clientset.Interface, ns, tcName, passwd string, tlsEnabled bool) wait.ConditionFunc {
	return func() (bool, error) {
		db, cancel, err := connectToTiDBWithTLSSupport(fw, c, ns, tcName, passwd, tlsEnabled)
		if err != nil {
			framework.Logf("failed to connect to source db: %v", err)
			return false, nil
		}
		defer db.Close()
		defer cancel()

		res, err := db.Exec("CREATE TABLE test.city (name VARCHAR(64) PRIMARY KEY)")
		if err != nil {
			framework.Logf("can't create table in source db: %v, %v", res, err)
			return false, nil
		}

		res, err = db.Exec("INSERT INTO test.city (name) VALUES (\"beijing\")")
		if err != nil {
			framework.Logf("can't insert into table tls in source db: %v, %v", res, err)
			return false, nil
		}

		return true, nil
	}
}

func dataInClusterIsCorrect(fw portforward.PortForward, c clientset.Interface, ns, tcName, passwd string, tlsEnabled bool) wait.ConditionFunc {
	return func() (bool, error) {
		db, cancel, err := connectToTiDBWithTLSSupport(fw, c, ns, tcName, passwd, tlsEnabled)
		if err != nil {
			framework.Logf("can't connect to %s/%s, %v", ns, tcName, err)
			return false, nil
		}
		defer db.Close()
		defer cancel()

		row := db.QueryRow("SELECT name from test.city limit 1")
		var name string

		err = row.Scan(&name)
		if err != nil {
			framework.Logf("can't scan from %s/%s, %v", ns, tcName, err)
			return false, nil
		}

		framework.Logf("TABLE test.city name = %s", name)
		if name == "beijing" {
			return true, nil
		}

		return false, nil
	}
}

func connectToTiDBWithTLSSupport(fw portforward.PortForward, c clientset.Interface, ns, tcName, passwd string, tlsEnabled bool) (*sql.DB, context.CancelFunc, error) {
	var tlsParams string

	localHost, localPort, cancel, err := portforward.ForwardOnePort(fw, ns, fmt.Sprintf("svc/%s", controller.TiDBMemberName(tcName)), 4000)
	if err != nil {
		return nil, nil, err
	}

	if tlsEnabled {
		tlsKey := "tidb-server-tls"
		secretName := util.TiDBClientTLSSecretName(tcName, nil)
		secret, err := c.CoreV1().Secrets(ns).Get(context.TODO(), secretName, metav1.GetOptions{})
		if err != nil {
			return nil, nil, err
		}

		rootCAs := x509.NewCertPool()
		rootCAs.AppendCertsFromPEM(secret.Data[v1.ServiceAccountRootCAKey])

		clientCert, certExists := secret.Data[v1.TLSCertKey]
		clientKey, keyExists := secret.Data[v1.TLSPrivateKeyKey]
		if !certExists || !keyExists {
			return nil, nil, fmt.Errorf("cert or key does not exist in secret %s/%s", ns, secretName)
		}

		tlsCert, err := tls.X509KeyPair(clientCert, clientKey)
		if err != nil {
			return nil, nil, fmt.Errorf("unable to load certificates from secret %s/%s: %v", ns, secretName, err)
		}
		err = mysql.RegisterTLSConfig(tlsKey, &tls.Config{
			RootCAs:            rootCAs,
			Certificates:       []tls.Certificate{tlsCert},
			InsecureSkipVerify: true,
		})
		if err != nil {
			return nil, nil, err
		}

		tlsParams = fmt.Sprintf("?tls=%s", tlsKey)
	}

	db, err := sql.Open("mysql",
		fmt.Sprintf("root:%s@(%s:%d)/test%s", passwd, localHost, localPort, tlsParams))
	if err != nil {
		return nil, nil, err
	}
	if err := db.Ping(); err != nil {
		return nil, nil, err
	}

	return db, cancel, err
}
