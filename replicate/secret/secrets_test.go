package secret

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mittwald/kubernetes-replicator/replicate/common"
	pkgerrors "github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

func namespacePrefix() string {
	//	Mon Jan 2 15:04:05 -0700 MST 2006
	return "test-repl-" + time.Now().Format("060102150405") + "-"
}

type EventHandlerFuncs struct {
	AddFunc    func(wg *sync.WaitGroup, obj interface{})
	UpdateFunc func(wg *sync.WaitGroup, oldObj, newObj interface{})
	DeleteFunc func(wg *sync.WaitGroup, obj interface{})
}

type PlainFormatter struct {
}

func (pf *PlainFormatter) Format(entry *log.Entry) ([]byte, error) {
	var b *bytes.Buffer
	if entry.Buffer != nil {
		b = entry.Buffer
	} else {
		b = &bytes.Buffer{}
	}

	b.WriteString(entry.Time.Format("15:04:05") + " ")
	b.WriteString(fmt.Sprintf("%-8s", strings.ToUpper(entry.Level.String())))
	b.WriteString(entry.Message)

	if val, ok := entry.Data[log.ErrorKey]; ok {
		b.WriteByte('\n')
		b.WriteString(fmt.Sprint(val))
	}

	b.WriteByte('\n')
	return b.Bytes(), nil
}

func TestSecretReplicator(t *testing.T) {

	log.SetLevel(log.TraceLevel)
	log.SetFormatter(&PlainFormatter{})

	configFile := os.Getenv("KUBECONFIG")
	config, err := clientcmd.BuildConfigFromFlags("", configFile)
	require.NoError(t, err)

	prefix := namespacePrefix()
	client := kubernetes.NewForConfigOrDie(config)

	repl := NewReplicator(client, 60*time.Second, false, false)
	go repl.Run()

	time.Sleep(200 * time.Millisecond)

	ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: prefix + "test"}}
	_, err = client.CoreV1().Namespaces().Create(&ns)
	require.NoError(t, err)

	ns2 := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: prefix + "test2"}}
	_, err = client.CoreV1().Namespaces().Create(&ns2)
	require.NoError(t, err)

	defer func() {
		_ = client.CoreV1().Namespaces().Delete(ns.Name, &metav1.DeleteOptions{})
		_ = client.CoreV1().Namespaces().Delete(ns2.Name, &metav1.DeleteOptions{})
	}()

	secrets := client.CoreV1().Secrets(prefix + "test")

	const MaxWaitTime = 1000 * time.Millisecond
	t.Run("replicates from existing secret", func(t *testing.T) {
		source := corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "source",
				Namespace: ns.Name,
				Annotations: map[string]string{
					common.ReplicationAllowed:           "true",
					common.ReplicationAllowedNamespaces: ns.Name,
				},
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{
				"foo": []byte("Hello World"),
			},
		}

		target := corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "target",
				Namespace: ns.Name,
				Annotations: map[string]string{
					common.ReplicateFromAnnotation: common.MustGetKey(&source),
				},
			},
			Type: corev1.SecretTypeOpaque,
		}

		wg, stop := waitForSecrets(client, 3, EventHandlerFuncs{
			AddFunc: func(wg *sync.WaitGroup, obj interface{}) {
				secret := obj.(*corev1.Secret)
				if secret.Namespace == source.Namespace && secret.Name == source.Name {
					log.Debugf("AddFunc %+v", obj)
					wg.Done()
				} else if secret.Namespace == target.Namespace && secret.Name == target.Name {
					log.Debugf("AddFunc %+v", obj)
					wg.Done()
				}
			},
			UpdateFunc: func(wg *sync.WaitGroup, oldObj interface{}, newObj interface{}) {
				secret := oldObj.(*corev1.Secret)
				if secret.Namespace == target.Namespace && secret.Name == target.Name {
					log.Debugf("UpdateFunc %+v -> %+v", oldObj, newObj)
					wg.Done()
				}
			},
		})

		_, err := secrets.Create(&source)
		require.NoError(t, err)

		_, err = secrets.Create(&target)
		require.NoError(t, err)

		waitWithTimeout(wg, MaxWaitTime)
		close(stop)

		updTarget, err := secrets.Get(target.Name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, []byte("Hello World"), updTarget.Data["foo"])
	})

	t.Run("replicates honours ReplicationAllowed tag", func(t *testing.T) {
		source := corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "source-repl-allowed",
				Namespace: ns.Name,
				Annotations: map[string]string{
					common.ReplicationAllowed:           "false",
					common.ReplicationAllowedNamespaces: ns2.Name,
				},
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{
				"foo": []byte("Hello World"),
			},
		}

		target := corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "target-repl-allowed",
				Namespace: ns2.Name,
				Annotations: map[string]string{
					common.ReplicateFromAnnotation: common.MustGetKey(&source),
				},
			},
			Type: corev1.SecretTypeOpaque,
		}

		wg, stop := waitForSecrets(client, 2, EventHandlerFuncs{
			AddFunc: func(wg *sync.WaitGroup, obj interface{}) {
				secret := obj.(*corev1.Secret)
				if secret.Namespace == source.Namespace && secret.Name == source.Name {
					log.Debugf("AddFunc %+v", obj)
					wg.Done()
				} else if secret.Namespace == target.Namespace && secret.Name == target.Name {
					log.Debugf("AddFunc %+v", obj)
					wg.Done()
				}
			},
		})

		_, err := secrets.Create(&source)
		require.NoError(t, err)

		secrets2 := client.CoreV1().Secrets(prefix + "test2")
		_, err = secrets2.Create(&target)
		require.NoError(t, err)

		waitWithTimeout(wg, MaxWaitTime)
		close(stop)

		updTarget, err := secrets2.Get(target.Name, metav1.GetOptions{})
		require.NoError(t, err)
		require.NotEqual(t, []byte("Hello World"), updTarget.Data["foo"])
	})

	t.Run("replicates keeps originally present values", func(t *testing.T) {
		source := corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "source3",
				Namespace: ns.Name,
				Annotations: map[string]string{
					common.ReplicationAllowed:           "true",
					common.ReplicationAllowedNamespaces: ns.Name,
				},
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{
				"foo": []byte("Hello World"),
			},
		}

		target := corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "target3",
				Namespace: ns.Name,
				Annotations: map[string]string{
					common.ReplicateFromAnnotation: common.MustGetKey(&source),
				},
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{
				"bar": []byte("Hello Bar"),
			},
		}

		wg, stop := waitForSecrets(client, 3, EventHandlerFuncs{
			AddFunc: func(wg *sync.WaitGroup, obj interface{}) {
				secret := obj.(*corev1.Secret)
				if secret.Namespace == source.Namespace && secret.Name == source.Name {
					log.Debugf("AddFunc %+v", obj)
					wg.Done()
				} else if secret.Namespace == target.Namespace && secret.Name == target.Name {
					log.Debugf("AddFunc %+v", obj)
					wg.Done()
				}
			},
			UpdateFunc: func(wg *sync.WaitGroup, oldObj interface{}, newObj interface{}) {
				secret := oldObj.(*corev1.Secret)
				if secret.Namespace == target.Namespace && secret.Name == target.Name {
					log.Debugf("UpdateFunc %+v -> %+v", oldObj, newObj)
					wg.Done()
				}
			},
		})
		_, err := secrets.Create(&source)
		require.NoError(t, err)

		_, err = secrets.Create(&target)
		require.NoError(t, err)

		waitWithTimeout(wg, MaxWaitTime)
		close(stop)

		updTarget, err := secrets.Get(target.Name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, []byte("Hello World"), updTarget.Data["foo"])
		require.Equal(t, []byte("Hello Bar"), updTarget.Data["bar"])
	})

	t.Run("replication removes keys removed from source secret", func(t *testing.T) {
		source := corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "source2",
				Namespace: ns.Name,
				Annotations: map[string]string{
					common.ReplicationAllowed:           "true",
					common.ReplicationAllowedNamespaces: ns.Name,
				},
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{
				"foo": []byte("Hello Foo"),
				"bar": []byte("Hello Bar"),
			},
		}

		target := corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "target2",
				Namespace: ns.Name,
				Annotations: map[string]string{
					common.ReplicateFromAnnotation: common.MustGetKey(&source),
				},
			},
			Type: corev1.SecretTypeOpaque,
		}

		wg, stop := waitForSecrets(client, 3, EventHandlerFuncs{
			AddFunc: func(wg *sync.WaitGroup, obj interface{}) {
				secret := obj.(*corev1.Secret)
				if secret.Namespace == source.Namespace && secret.Name == source.Name {
					log.Debugf("AddFunc %+v", obj)
					wg.Done()
				} else if secret.Namespace == target.Namespace && secret.Name == target.Name {
					log.Debugf("AddFunc %+v", obj)
					wg.Done()
				}
			},
			UpdateFunc: func(wg *sync.WaitGroup, oldObj interface{}, newObj interface{}) {
				secret := oldObj.(*corev1.Secret)
				if secret.Namespace == target.Namespace && secret.Name == target.Name {
					log.Debugf("UpdateFunc %+v -> %+v", oldObj, newObj)
					wg.Done()
				}
			},
		})

		_, err := secrets.Create(&source)
		require.NoError(t, err)

		_, err = secrets.Create(&target)
		require.NoError(t, err)

		waitWithTimeout(wg, MaxWaitTime)
		close(stop)

		updTarget, err := secrets.Get(target.Name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, []byte("Hello Foo"), updTarget.Data["foo"])

		wg, stop = waitForSecrets(client, 1, EventHandlerFuncs{
			UpdateFunc: func(wg *sync.WaitGroup, oldObj interface{}, newObj interface{}) {
				secret := oldObj.(*corev1.Secret)
				if secret.Namespace == target.Namespace && secret.Name == target.Name {
					log.Debugf("UpdateFunc %+v -> %+v", oldObj, newObj)
					wg.Done()
				}
			},
		})

		_, err = secrets.Patch(source.Name, types.JSONPatchType, []byte(`[{"op": "remove", "path": "/data/foo"}]`))
		require.NoError(t, err)

		waitWithTimeout(wg, MaxWaitTime)
		close(stop)

		updTarget, err = secrets.Get(target.Name, metav1.GetOptions{})
		require.NoError(t, err)

		_, hasFoo := updTarget.Data["foo"]
		require.False(t, hasFoo)
	})

	t.Run("replication does not remove original values", func(t *testing.T) {
		source := corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "source4",
				Namespace: ns.Name,
				Annotations: map[string]string{
					common.ReplicationAllowed:           "true",
					common.ReplicationAllowedNamespaces: ns.Name,
				},
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{
				"foo": []byte("Hello Foo"),
				"bar": []byte("Hello Bar"),
			},
		}

		target := corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "target4",
				Namespace: ns.Name,
				Annotations: map[string]string{
					common.ReplicateFromAnnotation: common.MustGetKey(&source),
				},
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{
				"bar": []byte("Hello Bar"),
			},
		}

		wg, stop := waitForSecrets(client, 3, EventHandlerFuncs{
			AddFunc: func(wg *sync.WaitGroup, obj interface{}) {
				secret := obj.(*corev1.Secret)
				if secret.Namespace == source.Namespace && secret.Name == source.Name {
					log.Debugf("AddFunc %+v", obj)
					wg.Done()
				} else if secret.Namespace == target.Namespace && secret.Name == target.Name {
					log.Debugf("AddFunc %+v", obj)
					wg.Done()
				}
			},
			UpdateFunc: func(wg *sync.WaitGroup, oldObj interface{}, newObj interface{}) {
				secret := oldObj.(*corev1.Secret)
				if secret.Namespace == target.Namespace && secret.Name == target.Name {
					log.Debugf("UpdateFunc %+v -> %+v", oldObj, newObj)
					wg.Done()
				}
			},
		})

		_, err := secrets.Create(&source)
		require.NoError(t, err)

		_, err = secrets.Create(&target)
		require.NoError(t, err)

		waitWithTimeout(wg, MaxWaitTime)
		close(stop)

		updTarget, err := secrets.Get(target.Name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, []byte("Hello Foo"), updTarget.Data["foo"])

		wg, stop = waitForSecrets(client, 1, EventHandlerFuncs{
			UpdateFunc: func(wg *sync.WaitGroup, oldObj interface{}, newObj interface{}) {
				secret := oldObj.(*corev1.Secret)
				if secret.Namespace == target.Namespace && secret.Name == target.Name {
					log.Debugf("UpdateFunc %+v -> %+v", oldObj, newObj)
					wg.Done()
				}
			},
		})

		_, err = secrets.Patch(source.Name, types.JSONPatchType, []byte(`[{"op": "remove", "path": "/data/foo"}]`))
		require.NoError(t, err)

		waitWithTimeout(wg, MaxWaitTime)
		close(stop)

		updTarget, err = secrets.Get(target.Name, metav1.GetOptions{})
		require.NoError(t, err)

		_, hasFoo := updTarget.Data["foo"]
		require.False(t, hasFoo)
		require.Equal(t, []byte("Hello Bar"), updTarget.Data["bar"])
	})

	t.Run("replication is pushed to other namespaces", func(t *testing.T) {
		source := corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "source-pushed-to-other-ns",
				Namespace: ns.Name,
				Annotations: map[string]string{
					common.ReplicateTo: prefix + "test2",
				},
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{
				"foo": []byte("Hello Foo"),
				"bar": []byte("Hello Bar"),
			},
		}

		wg, stop := waitForSecrets(client, 2, EventHandlerFuncs{
			AddFunc: func(wg *sync.WaitGroup, obj interface{}) {
				secret := obj.(*corev1.Secret)
				if secret.Namespace == source.Namespace && secret.Name == source.Name {
					log.Debugf("AddFunc %+v", obj)
					wg.Done()
				} else if secret.Namespace == prefix+"test2" && secret.Name == source.Name {
					log.Debugf("AddFunc %+v", obj)
					wg.Done()
				}
			},
		})
		_, err := secrets.Create(&source)
		require.NoError(t, err)

		waitWithTimeout(wg, MaxWaitTime)
		close(stop)

		secrets2 := client.CoreV1().Secrets(prefix + "test2")
		updTarget, err := secrets2.Get(source.Name, metav1.GetOptions{})

		require.NoError(t, err)
		require.Equal(t, []byte("Hello Foo"), updTarget.Data["foo"])

		wg, stop = waitForSecrets(client, 1, EventHandlerFuncs{
			UpdateFunc: func(wg *sync.WaitGroup, oldObj interface{}, newObj interface{}) {
				secret := oldObj.(*corev1.Secret)
				if secret.Namespace == prefix+"test2" && secret.Name == source.Name {
					log.Debugf("UpdateFunc %+v -> %+v", oldObj, newObj)
					wg.Done()
				}
			},
		})

		_, err = secrets.Patch(source.Name, types.JSONPatchType, []byte(`[{"op": "remove", "path": "/data/foo"}]`))
		require.NoError(t, err)

		waitWithTimeout(wg, MaxWaitTime)
		close(stop)

		updTarget, err = secrets2.Get(source.Name, metav1.GetOptions{})
		require.NoError(t, err)

		_, hasFoo := updTarget.Data["foo"]
		require.False(t, hasFoo)
		require.Equal(t, []byte("Hello Bar"), updTarget.Data["bar"])
	})

	t.Run("replication updates existing secrets", func(t *testing.T) {
		secrets2 := client.CoreV1().Secrets(prefix + "test2")

		target := corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "source-repl-updates-existing",
				Namespace: ns2.Name,
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{},
		}

		_, err = secrets2.Create(&target)
		require.NoError(t, err)

		time.Sleep(100 * time.Millisecond)

		source := corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "source-repl-updates-existing",
				Namespace: ns.Name,
				Annotations: map[string]string{
					common.ReplicateTo: prefix + "test2",
				},
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{
				"foo": []byte("Hello Foo"),
				"bar": []byte("Hello Bar"),
			},
		}

		_, err := secrets.Create(&source)
		require.NoError(t, err)

		time.Sleep(300 * time.Millisecond)

		updTarget, err := secrets2.Get(source.Name, metav1.GetOptions{})

		require.NoError(t, err)
		require.Equal(t, []byte("Hello Foo"), updTarget.Data["foo"])

		_, err = secrets.Patch(source.Name, types.JSONPatchType, []byte(`[{"op": "remove", "path": "/data/foo"}]`))
		require.NoError(t, err)

		time.Sleep(300 * time.Millisecond)

		updTarget, err = secrets2.Get(source.Name, metav1.GetOptions{})
		require.NoError(t, err)

		_, hasFoo := updTarget.Data["foo"]
		require.False(t, hasFoo)
		require.Equal(t, []byte("Hello Bar"), updTarget.Data["bar"])
	})

	t.Run("secrets are replicated when new namespace is created", func(t *testing.T) {
		namespaceName := prefix + "test-repl-new-ns"
		source := corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "source6",
				Namespace: ns.Name,
				Annotations: map[string]string{
					common.ReplicateTo: namespaceName,
				},
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{
				"foo": []byte("Hello Foo"),
				"bar": []byte("Hello Bar"),
			},
		}

		wg, stop := waitForSecrets(client, 1, EventHandlerFuncs{
			AddFunc: func(wg *sync.WaitGroup, obj interface{}) {
				secret := obj.(*corev1.Secret)
				if secret.Namespace == source.Namespace && secret.Name == source.Name {
					log.Debugf("AddFunc %+v", obj)
					wg.Done()
				}
			},
		})

		_, err := secrets.Create(&source)
		require.NoError(t, err)

		waitWithTimeout(wg, MaxWaitTime)
		close(stop)

		ns3 := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespaceName}}

		wg, stop = waitForNamespaces(client, 1, EventHandlerFuncs{
			AddFunc: func(wg *sync.WaitGroup, obj interface{}) {
				ns := obj.(*corev1.Namespace)
				if ns.Name == ns3.Name {
					log.Debugf("AddFunc %+v", obj)
					wg.Done()
				}
			},
		})

		wg2, stop2 := waitForSecrets(client, 1, EventHandlerFuncs{
			AddFunc: func(wg *sync.WaitGroup, obj interface{}) {
				secret := obj.(*corev1.Secret)
				if secret.Namespace == ns3.Name && secret.Name == source.Name {
					log.Debugf("AddFunc %+v", obj)
					wg.Done()
				}
			},
		})

		_, err = client.CoreV1().Namespaces().Create(&ns3)
		require.NoError(t, err)

		defer func() {
			_ = client.CoreV1().Namespaces().Delete(ns3.Name, &metav1.DeleteOptions{})
		}()

		waitWithTimeout(wg, MaxWaitTime)
		close(stop)

		waitWithTimeout(wg2, MaxWaitTime)
		close(stop2)

		secrets3 := client.CoreV1().Secrets(namespaceName)
		updTarget, err := secrets3.Get(source.Name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, []byte("Hello Foo"), updTarget.Data["foo"])

		wg, stop = waitForSecrets(client, 1, EventHandlerFuncs{
			UpdateFunc: func(wg *sync.WaitGroup, objOld interface{}, objNew interface{}) {
				secret := objOld.(*corev1.Secret)
				if secret.Namespace == ns3.Name && secret.Name == source.Name {
					log.Debugf("UpdateFunc %+v", objOld)
					wg.Done()
				}
			},
		})
		_, err = secrets.Patch(source.Name, types.JSONPatchType, []byte(`[{"op": "remove", "path": "/data/foo"}]`))
		require.NoError(t, err)

		waitWithTimeout(wg, MaxWaitTime)
		close(stop)

		updTarget, err = secrets3.Get(source.Name, metav1.GetOptions{})
		require.NoError(t, err)

		_, hasFoo := updTarget.Data["foo"]
		require.False(t, hasFoo)
		require.Equal(t, []byte("Hello Bar"), updTarget.Data["bar"])
	})

	t.Run("secrets updated when namespace is deleted", func(t *testing.T) {
		ns4 := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: prefix + "test4"}}

		wg, stop := waitForNamespaces(client, 1, EventHandlerFuncs{
			AddFunc: func(wg *sync.WaitGroup, obj interface{}) {
				ns := obj.(*corev1.Namespace)
				if ns.Name == ns4.Name {
					log.Debugf("AddFunc %+v", obj)
					wg.Done()
				}
			},
		})

		_, err = client.CoreV1().Namespaces().Create(&ns4)
		require.NoError(t, err)

		waitWithTimeout(wg, MaxWaitTime)
		close(stop)

		source := corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "source-ns-delete",
				Namespace: ns4.Name,
				Annotations: map[string]string{
					common.ReplicationAllowed:           "true",
					common.ReplicationAllowedNamespaces: ns.Name,
				},
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{
				"foo": []byte("Hello Foo"),
				"bar": []byte("Hello Bar"),
			},
		}

		target := corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "target-ns-delete",
				Namespace: ns.Name,
				Annotations: map[string]string{
					common.ReplicateFromAnnotation: common.MustGetKey(&source),
				},
			},
			Type: corev1.SecretTypeOpaque,
		}

		wg, stop = waitForSecrets(client, 3, EventHandlerFuncs{
			AddFunc: func(wg *sync.WaitGroup, obj interface{}) {
				secret := obj.(*corev1.Secret)
				if secret.Namespace == source.Namespace && secret.Name == source.Name {
					log.Debugf("AddFunc %+v", obj)
					wg.Done()
				} else if secret.Namespace == target.Namespace && secret.Name == target.Name {
					log.Debugf("AddFunc %+v", obj)
					wg.Done()
				}
			},
			UpdateFunc: func(wg *sync.WaitGroup, oldObj interface{}, newObj interface{}) {
				secret := oldObj.(*corev1.Secret)
				if secret.Namespace == target.Namespace && secret.Name == target.Name {
					log.Debugf("UpdateFunc %+v -> %+v", oldObj, newObj)
					wg.Done()
				}
			},
		})

		secrets4 := client.CoreV1().Secrets(prefix + "test4")

		_, err := secrets4.Create(&source)
		require.NoError(t, err)

		_, err = secrets.Create(&target)
		require.NoError(t, err)

		waitWithTimeout(wg, MaxWaitTime)
		close(stop)

		wg, stop = waitForNamespaces(client, 1, EventHandlerFuncs{
			DeleteFunc: func(wg *sync.WaitGroup, obj interface{}) {
				ns := obj.(*corev1.Namespace)
				if ns.Name == ns4.Name {
					log.Debugf("DeleteFunc %+v", obj)
					wg.Done()
				}
			},
		})

		err = client.CoreV1().Namespaces().Delete(ns4.Name, &metav1.DeleteOptions{})
		require.NoError(t, err)

		waitWithTimeout(wg, MaxWaitTime*10)
		close(stop)

		nsfound, err := client.CoreV1().Namespaces().Get(ns4.Name, metav1.GetOptions{})
		require.Condition(t, func() bool { return errors.IsNotFound(err) }, "Expected no namespace but got: %v; %v", nsfound, err)

		updTarget, err := secrets.Get(target.Name, metav1.GetOptions{})
		require.NoError(t, err)
		require.NotEqual(t, []byte("Hello Bar"), updTarget.Data["bar"])
	})

	t.Run("deleting a secret deletes it in other namespaces", func(t *testing.T) {
		source := corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "source7",
				Namespace: ns.Name,
				Annotations: map[string]string{
					common.ReplicateTo: prefix + "test2",
				},
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{
				"foo": []byte("Hello Foo"),
				"bar": []byte("Hello Bar"),
			},
		}

		wg, stop := waitForSecrets(client, 2, EventHandlerFuncs{
			AddFunc: func(wg *sync.WaitGroup, obj interface{}) {
				secret := obj.(*corev1.Secret)
				if secret.Namespace == source.Namespace && secret.Name == source.Name {
					log.Debugf("AddFunc %+v", obj)
					wg.Done()
				} else if secret.Namespace == prefix+"test2" && secret.Name == source.Name {
					log.Debugf("AddFunc %+v", obj)
					wg.Done()
				}
			},
		})

		_, err := secrets.Create(&source)
		require.NoError(t, err)

		waitWithTimeout(wg, MaxWaitTime)
		close(stop)

		secrets2 := client.CoreV1().Secrets(prefix + "test2")
		_, err = secrets2.Get(source.Name, metav1.GetOptions{})
		require.NoError(t, err)

		wg, stop = waitForSecrets(client, 2, EventHandlerFuncs{
			DeleteFunc: func(wg *sync.WaitGroup, obj interface{}) {
				secret := obj.(*corev1.Secret)
				if secret.Namespace == source.Namespace && secret.Name == source.Name {
					log.Debugf("DeleteFunc %+v", obj)
					wg.Done()
				} else if secret.Namespace == prefix+"test2" && secret.Name == source.Name {
					log.Debugf("DeleteFunc %+v", obj)
					wg.Done()
				}
			},
		})

		err = secrets.Delete(source.Name, &metav1.DeleteOptions{})
		require.NoError(t, err)

		waitWithTimeout(wg, MaxWaitTime)
		close(stop)

		_, err = secrets.Get(source.Name, metav1.GetOptions{})
		require.Condition(t, func() bool { return errors.IsNotFound(err) }, "Expected not found, but got a secret in namespace test: %+v", err)

		_, err = secrets2.Get(source.Name, metav1.GetOptions{})
		require.Condition(t, func() bool { return errors.IsNotFound(err) }, "Expected not found, but got: %+v", err)
	})

	t.Run("replication properly replicates type", func(t *testing.T) {
		source := corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "source8",
				Namespace: ns.Name,
				Annotations: map[string]string{
					common.ReplicateTo: prefix + "test2",
				},
			},
			Type: corev1.SecretTypeDockercfg,
			Data: map[string][]byte{
				".dockerconfigjson": []byte("{}"),
				".dockercfg":        []byte("{}"),
			},
		}

		wg, stop := waitForSecrets(client, 2, EventHandlerFuncs{
			AddFunc: func(wg *sync.WaitGroup, obj interface{}) {
				secret := obj.(*corev1.Secret)
				if secret.Namespace == source.Namespace && secret.Name == source.Name {
					log.Debugf("AddFunc %+v", obj)
					wg.Done()
				} else if secret.Namespace == prefix+"test2" && secret.Name == source.Name {
					log.Debugf("AddFunc %+v", obj)
					wg.Done()
				}
			},
		})

		_, err := secrets.Create(&source)
		require.NoError(t, err)

		waitWithTimeout(wg, MaxWaitTime)
		close(stop)

		secrets2 := client.CoreV1().Secrets(prefix + "test2")
		updTarget, err := secrets2.Get(source.Name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, []byte("{}"), updTarget.Data[".dockercfg"])
		require.Equal(t, corev1.SecretTypeDockercfg, updTarget.Type)

	})

}

func TestSecretReplicatorStrict(t *testing.T) {

	log.SetLevel(log.TraceLevel)
	log.SetFormatter(&PlainFormatter{})

	configFile := os.Getenv("KUBECONFIG")
	config, err := clientcmd.BuildConfigFromFlags("", configFile)
	require.NoError(t, err)

	prefix := namespacePrefix()
	client := kubernetes.NewForConfigOrDie(config)

	repl := NewReplicator(client, 60*time.Second, false, true)
	go repl.Run()

	time.Sleep(200 * time.Millisecond)

	ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: prefix + "test"}}
	_, err = client.CoreV1().Namespaces().Create(&ns)
	require.NoError(t, err)

	ns2 := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: prefix + "test2"}}
	_, err = client.CoreV1().Namespaces().Create(&ns2)
	require.NoError(t, err)

	defer func() {
		_ = client.CoreV1().Namespaces().Delete(ns.Name, &metav1.DeleteOptions{})
		_ = client.CoreV1().Namespaces().Delete(ns2.Name, &metav1.DeleteOptions{})
	}()

	secrets := client.CoreV1().Secrets(prefix + "test")

	const MaxWaitTime = 1000 * time.Millisecond
	t.Run("enforce reference secret content equals source secret", func(t *testing.T) {
		source := corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "source",
				Namespace: ns.Name,
				Annotations: map[string]string{
					common.ReplicationAllowed:           "true",
					common.ReplicationAllowedNamespaces: ns.Name,
				},
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{
				"foo": []byte("Hello World"),
			},
		}

		target := corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "target",
				Namespace: ns.Name,
				Annotations: map[string]string{
					common.ReplicateFromAnnotation: common.MustGetKey(&source),
				},
			},
			Type: corev1.SecretTypeOpaque,
		}
		tmpOverwrite := corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "target",
				Namespace: ns.Name,
				Annotations: map[string]string{
					common.ReplicateFromAnnotation: common.MustGetKey(&source),
				},
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{
				"foo": []byte("manually changed secret"),
			},
		}

		wg, stop := waitForSecrets(client, 6, EventHandlerFuncs{
			AddFunc: func(wg *sync.WaitGroup, obj interface{}) {
				secret := obj.(*corev1.Secret)
				if secret.Namespace == source.Namespace && secret.Name == source.Name {
					log.Debugf("AddFunc %+v", obj)
					wg.Done()
				} else if secret.Namespace == target.Namespace && secret.Name == target.Name {
					log.Debugf("AddFunc %+v", obj)
					wg.Done()
				}
			},
			UpdateFunc: func(wg *sync.WaitGroup, oldObj interface{}, newObj interface{}) {
				secret := oldObj.(*corev1.Secret)
				if secret.Namespace == target.Namespace && secret.Name == target.Name {
					log.Debugf("UpdateFunc %+v -> %+v", oldObj, newObj)
					wg.Done()
				}
			},
		})

		_, err := secrets.Create(&source)
		require.NoError(t, err)

		_, err = secrets.Create(&target)
		require.NoError(t, err)

		waitWithTimeout(wg, MaxWaitTime)

		updTarget, err := secrets.Get(target.Name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, []byte("Hello World"), updTarget.Data["foo"])

		_, err = secrets.Update(&tmpOverwrite)
		require.NoError(t, err)

		waitWithTimeout(wg, MaxWaitTime)

		updTarget, err = secrets.Get(target.Name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, []byte("Hello World"), updTarget.Data["foo"])

		close(stop)
	})

}

func waitForNamespaces(client *kubernetes.Clientset, count int, eventHandlers EventHandlerFuncs) (wg *sync.WaitGroup, stop chan struct{}) {
	wg = &sync.WaitGroup{}
	wg.Add(count)
	informerFactory := informers.NewSharedInformerFactory(client, 60*time.Second)
	informer := informerFactory.Core().V1().Namespaces().Informer()
	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if eventHandlers.AddFunc != nil {
				eventHandlers.AddFunc(wg, obj)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			if eventHandlers.UpdateFunc != nil {
				eventHandlers.UpdateFunc(wg, oldObj, newObj)
			}

		},
		DeleteFunc: func(obj interface{}) {
			if eventHandlers.DeleteFunc != nil {
				eventHandlers.DeleteFunc(wg, obj)
			}
		},
	})
	stop = make(chan struct{})
	go informerFactory.Start(stop)

	return

}

func waitForSecrets(client *kubernetes.Clientset, count int, eventHandlers EventHandlerFuncs) (wg *sync.WaitGroup, stop chan struct{}) {
	wg = &sync.WaitGroup{}
	wg.Add(count)
	informerFactory := informers.NewSharedInformerFactory(client, 60*time.Second)
	informer := informerFactory.Core().V1().Secrets().Informer()
	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if eventHandlers.AddFunc != nil {
				eventHandlers.AddFunc(wg, obj)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			if eventHandlers.UpdateFunc != nil {
				eventHandlers.UpdateFunc(wg, oldObj, newObj)
			}

		},
		DeleteFunc: func(obj interface{}) {
			if eventHandlers.DeleteFunc != nil {
				eventHandlers.DeleteFunc(wg, obj)
			}
		},
	})
	stop = make(chan struct{})
	go informerFactory.Start(stop)

	return

}

func waitWithTimeout(wg *sync.WaitGroup, timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return
	case <-time.After(timeout):
		err := pkgerrors.Errorf("Timeout hit")
		log.WithError(err).Debugf("Wait timed out")
	}
}
