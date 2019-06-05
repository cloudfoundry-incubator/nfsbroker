package main

import (
	"errors"
	"io"
	"net/http"
	"os/exec"
	"strconv"

	"encoding/json"
	"io/ioutil"

	"fmt"

	"os"
	"time"

	"github.com/onsi/gomega/gbytes"
	"github.com/onsi/gomega/gexec"
	"github.com/pivotal-cf/brokerapi"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/ginkgomon"

	"code.cloudfoundry.org/goshims/osshim/os_fake"
	"code.cloudfoundry.org/lager"
	"code.cloudfoundry.org/lager/lagertest"
	"code.cloudfoundry.org/nfsbroker/fakes"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

type failRunner struct {
	Command           *exec.Cmd
	Name              string
	AnsiColorCode     string
	StartCheck        string
	StartCheckTimeout time.Duration
	Cleanup           func()
	session           *gexec.Session
	sessionReady      chan struct{}
}

func (r failRunner) Run(sigChan <-chan os.Signal, ready chan<- struct{}) error {
	defer GinkgoRecover()

	allOutput := gbytes.NewBuffer()

	debugWriter := gexec.NewPrefixedWriter(
		fmt.Sprintf("\x1b[32m[d]\x1b[%s[%s]\x1b[0m ", r.AnsiColorCode, r.Name),
		GinkgoWriter,
	)

	session, err := gexec.Start(
		r.Command,
		gexec.NewPrefixedWriter(
			fmt.Sprintf("\x1b[32m[o]\x1b[%s[%s]\x1b[0m ", r.AnsiColorCode, r.Name),
			io.MultiWriter(allOutput, GinkgoWriter),
		),
		gexec.NewPrefixedWriter(
			fmt.Sprintf("\x1b[91m[e]\x1b[%s[%s]\x1b[0m ", r.AnsiColorCode, r.Name),
			io.MultiWriter(allOutput, GinkgoWriter),
		),
	)

	Ω(err).ShouldNot(HaveOccurred())

	fmt.Fprintf(debugWriter, "spawned %s (pid: %d)\n", r.Command.Path, r.Command.Process.Pid)

	r.session = session
	if r.sessionReady != nil {
		close(r.sessionReady)
	}

	startCheckDuration := r.StartCheckTimeout
	if startCheckDuration == 0 {
		startCheckDuration = 5 * time.Second
	}

	var startCheckTimeout <-chan time.Time
	if r.StartCheck != "" {
		startCheckTimeout = time.After(startCheckDuration)
	}

	detectStartCheck := allOutput.Detect(r.StartCheck)

	for {
		select {
		case <-detectStartCheck: // works even with empty string
			allOutput.CancelDetects()
			startCheckTimeout = nil
			detectStartCheck = nil
			close(ready)

		case <-startCheckTimeout:
			// clean up hanging process
			session.Kill().Wait()

			// fail to start
			return fmt.Errorf(
				"did not see %s in command's output within %s. full output:\n\n%s",
				r.StartCheck,
				startCheckDuration,
				string(allOutput.Contents()),
			)

		case signal := <-sigChan:
			session.Signal(signal)

		case <-session.Exited:
			if r.Cleanup != nil {
				r.Cleanup()
			}

			Expect(string(allOutput.Contents())).To(ContainSubstring(r.StartCheck))
			Expect(session.ExitCode()).To(Not(Equal(0)), fmt.Sprintf("Expected process to exit with non-zero, got: 0"))
			return nil
		}
	}
}

var _ = Describe("nfsbroker Main", func() {
	Context("Parse VCAP_SERVICES tests", func() {
		var (
			port   string
			fakeOs os_fake.FakeOs = os_fake.FakeOs{}
			logger lager.Logger
		)

		BeforeEach(func() {
			*dbDriver = "postgres"
			*cfServiceName = "postgresql"
			logger = lagertest.NewTestLogger("test-broker-main")
		})
		JustBeforeEach(func() {
			env := fmt.Sprintf(`
				{
					"postgresql":[
						{
							"credentials":{
								"dbType":"postgresql",
								"hostname":"8.8.8.8",
								"name":"foo",
								"password":"foo",
								"port":%s,
								"uri":"postgresql://foo:foo@8.8.8.8:9999/foo",
								"username":"foo"
							},
							"label":"postgresql",
							"name":"foobroker",
							"plan":"amanaplanacanalpanama",
							"provider":null,
							"syslog_drain_url":null,
							"tags":[
								"postgresql",
								"cache"
							],
							"volume_mounts":[]
						}
					]
				}`, port)
			fakeOs.LookupEnvReturns(env, true)
		})

		Context("when port is a string", func() {
			BeforeEach(func() {
				port = `"9999"`
			})

			It("should succeed", func() {
				Expect(func() { parseVcapServices(logger, &fakeOs) }).NotTo(Panic())
				Expect(*dbPort).To(Equal("9999"))
			})
		})
		Context("when port is a number", func() {
			BeforeEach(func() {
				port = `9999`
			})

			It("should succeed", func() {
				Expect(func() { parseVcapServices(logger, &fakeOs) }).NotTo(Panic())
				Expect(*dbPort).To(Equal("9999"))
			})
		})
		Context("when port is an array", func() {
			BeforeEach(func() {
				port = `[9999]`
			})

			It("should panic", func() {
				Expect(func() { parseVcapServices(logger, &fakeOs) }).To(Panic())
			})
		})
	})

	Context("Missing required args", func() {
		var process ifrit.Process

		It("shows usage when dataDir or dbDriver are not provided", func() {
			var args []string
			volmanRunner := failRunner{
				Name:       "nfsbroker",
				Command:    exec.Command(binaryPath, args...),
				StartCheck: "Either dataDir, dbDriver or credhubURL parameters must be provided.",
			}
			process = ifrit.Invoke(volmanRunner)
		})

		It("shows usage when servicesConfig is not provided", func() {
			args := []string{"-dbDriver", "mysql"}
			volmanRunner := failRunner{
				Name:       "nfsbroker",
				Command:    exec.Command(binaryPath, args...),
				StartCheck: "servicesConfig parameter must be provided.",
			}
			process = ifrit.Invoke(volmanRunner)
		})

		AfterEach(func() {
			ginkgomon.Kill(process) // this is only if incorrect implementation leaves process running
		})
	})

	Context("Has required args", func() {
		var (
			args               []string
			listenAddr         string
			tempDir            string
			username, password string

			process ifrit.Process
		)

		BeforeEach(func() {
			listenAddr = "0.0.0.0:" + strconv.Itoa(7999+GinkgoParallelNode())
			username = "admin"
			password = "password"
			tempDir = os.TempDir()

			os.Setenv("USERNAME", username)
			os.Setenv("PASSWORD", password)

			args = append(args, "-credhubURL", "https://localhost:9000")
			args = append(args, "-credhubCACertPath", "/tmp/server_ca_cert.pem")

			args = append(args, "-uaaClientID", "credhub_client")
			args = append(args, "-uaaClientSecret", "secret")
			args = append(args, "-listenAddr", listenAddr)
			args = append(args, "-dataDir", tempDir)
			args = append(args, "-servicesConfig", "./default_services.json")
		})

		JustBeforeEach(func() {
			volmanRunner := ginkgomon.New(ginkgomon.Config{
				Name:       "nfsbroker",
				Command:    exec.Command(binaryPath, args...),
				StartCheck: "started",
			})
			process = ginkgomon.Invoke(volmanRunner)
		})

		AfterEach(func() {
			ginkgomon.Kill(process)
		})

		httpDoWithAuth := func(method, endpoint string, body io.ReadCloser) (*http.Response, error) {
			req, err := http.NewRequest(method, "http://"+listenAddr+endpoint, body)
			req.Header.Add("X-Broker-Api-Version", "2.14")
			Expect(err).NotTo(HaveOccurred())

			req.SetBasicAuth(username, password)
			return http.DefaultClient.Do(req)
		}

		It("should listen on the given address", func() {
			resp, err := httpDoWithAuth("GET", "/v2/catalog", nil)
			Expect(err).NotTo(HaveOccurred())

			Expect(resp.StatusCode).To(Equal(200))
		})

		It("should pass services config through to catalog", func() {
			resp, err := httpDoWithAuth("GET", "/v2/catalog", nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(200))

			bytes, err := ioutil.ReadAll(resp.Body)
			Expect(err).NotTo(HaveOccurred())

			var catalog brokerapi.CatalogResponse
			err = json.Unmarshal(bytes, &catalog)
			Expect(err).NotTo(HaveOccurred())

			Expect(catalog.Services).To(HaveLen(2))

			Expect(catalog.Services[0].Name).To(Equal("nfs-legacy"))
			Expect(catalog.Services[0].ID).To(Equal("nfsbroker"))
			Expect(catalog.Services[0].Plans[0].ID).To(Equal("Existing"))
			Expect(catalog.Services[0].Plans[0].Name).To(Equal("Existing"))
			Expect(catalog.Services[0].Plans[0].Description).To(Equal("A preexisting filesystem"))

			Expect(catalog.Services[1].Name).To(Equal("nfs"))
			Expect(catalog.Services[1].ID).To(Equal("997f8f26-e10c-11e7-80c1-9a214cf093ae"))
			Expect(catalog.Services[1].Plans[0].ID).To(Equal("09a09260-1df5-4445-9ed7-1ba56dadbbc8"))
			Expect(catalog.Services[1].Plans[0].Name).To(Equal("Existing"))
			Expect(catalog.Services[1].Plans[0].Description).To(Equal("A preexisting filesystem"))
		})
	})

	Context("#IsRetired", func() {
		var (
			fakeRetiredStore *fakes.FakeRetiredStore
			retired          bool
			err              error
		)

		JustBeforeEach(func() {
			retired, err = IsRetired(fakeRetiredStore)
		})

		BeforeEach(func() {
			fakeRetiredStore = &fakes.FakeRetiredStore{}
		})

		Context("when the store is not a RetireableStore", func() {
			BeforeEach(func() {
				fakeRetiredStore.IsRetiredReturns(false, nil)
			})

			It("should return false", func() {
				Expect(err).NotTo(HaveOccurred())
				Expect(retired).To(BeFalse())
			})
		})

		Context("when the store is a RetiredStore", func() {
			Context("when the store is retired", func() {
				BeforeEach(func() {
					fakeRetiredStore.IsRetiredReturns(true, nil)
				})

				It("should return true", func() {
					Expect(err).NotTo(HaveOccurred())
					Expect(retired).To(BeTrue())
				})
			})

			Context("when the store is not retired", func() {
				BeforeEach(func() {
					fakeRetiredStore.IsRetiredReturns(false, nil)
				})

				It("should return false", func() {
					Expect(err).NotTo(HaveOccurred())
					Expect(retired).To(BeFalse())
				})
			})

			Context("when the IsRetired check fails", func() {
				BeforeEach(func() {
					fakeRetiredStore.IsRetiredReturns(false, errors.New("is-retired-failed"))
				})

				It("should return true", func() {
					Expect(err).To(MatchError("is-retired-failed"))
				})
			})
		})
	})
})
