package visor

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SkycoinProject/dmsg/cipher"
	"github.com/SkycoinProject/skycoin/src/util/logging"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/SkycoinProject/skywire-mainnet/internal/testhelpers"
	"github.com/SkycoinProject/skywire-mainnet/pkg/app/appcommon"
	"github.com/SkycoinProject/skywire-mainnet/pkg/app/appserver"
	"github.com/SkycoinProject/skywire-mainnet/pkg/router"
	"github.com/SkycoinProject/skywire-mainnet/pkg/routing"
	"github.com/SkycoinProject/skywire-mainnet/pkg/util/pathutil"
)

func TestHealth(t *testing.T) {
	c := &Config{
		KeyPair: NewKeyPair(),
		Transport: &TransportConfig{
			Discovery: "foo",
		},
		Routing: &RoutingConfig{
			RouteFinder: "foo",
		},
	}

	c.Routing.SetupNodes = []cipher.PubKey{c.KeyPair.PubKey}

	t.Run("Report all the services as available", func(t *testing.T) {
		rpc := &RPC{visor: &Visor{conf: c}, log: logrus.New()}
		h := &HealthInfo{}
		err := rpc.Health(nil, h)
		require.NoError(t, err)

		// Transport discovery needs to be mocked or will always fail
		assert.Equal(t, http.StatusOK, h.SetupNode)
		assert.Equal(t, http.StatusOK, h.RouteFinder)
	})

	t.Run("Report as unavailable", func(t *testing.T) {
		conf := &Config{
			Routing: &RoutingConfig{},
		}

		rpc := &RPC{visor: &Visor{conf: conf}, log: logrus.New()}
		h := &HealthInfo{}
		err := rpc.Health(nil, h)
		require.NoError(t, err)

		assert.Equal(t, http.StatusNotFound, h.SetupNode)
		assert.Equal(t, http.StatusNotFound, h.RouteFinder)
	})
}

func TestUptime(t *testing.T) {
	rpc := &RPC{visor: &Visor{startedAt: time.Now()}, log: logrus.New()}
	time.Sleep(time.Second)

	var res float64
	err := rpc.Uptime(nil, &res)
	require.NoError(t, err)

	assert.Contains(t, fmt.Sprintf("%f", res), "1.0")
}

func TestListApps(t *testing.T) {
	apps := make(map[string]AppConfig)
	appCfg := []AppConfig{
		{
			App:       "foo",
			AutoStart: false,
			Port:      10,
		},
		{
			App:       "bar",
			AutoStart: true,
			Port:      11,
		},
	}

	for _, app := range appCfg {
		apps[app.App] = app
	}

	pm := &appserver.MockProcManager{}
	pm.On("Exists", apps["foo"].App).Return(false)
	pm.On("Exists", apps["bar"].App).Return(true)

	n := Visor{
		appsConf:    apps,
		procManager: pm,
	}

	rpc := &RPC{visor: &n, log: logrus.New()}

	var reply []*AppState
	require.NoError(t, rpc.Apps(nil, &reply))
	require.Len(t, reply, 2)

	app1, app2 := reply[0], reply[1]
	if app1.Name != "foo" {
		// apps inside visor are stored inside a map, so their order
		// is not deterministic, we should be ready for this and
		// rearrange the outer array to check values correctly
		app1, app2 = reply[1], reply[0]
	}

	assert.Equal(t, "foo", app1.Name)
	assert.False(t, app1.AutoStart)
	assert.Equal(t, routing.Port(10), app1.Port)
	assert.Equal(t, AppStatusStopped, app1.Status)

	assert.Equal(t, "bar", app2.Name)
	assert.True(t, app2.AutoStart)
	assert.Equal(t, routing.Port(11), app2.Port)
	assert.Equal(t, AppStatusRunning, app2.Status)
}

func TestStartStopApp(t *testing.T) {
	tempDir, err := ioutil.TempDir(os.TempDir(), "")
	require.NoError(t, err)
	defer func() { require.NoError(t, os.RemoveAll(tempDir)) }()

	r := &router.MockRouter{}
	r.On("Serve", mock.Anything /* context */).Return(testhelpers.NoErr)
	r.On("Close").Return(testhelpers.NoErr)

	defer func() {
		require.NoError(t, os.RemoveAll("skychat"))
	}()

	appCfg := []AppConfig{
		{
			App:       "foo",
			AutoStart: false,
			Port:      10,
		},
	}
	apps := map[string]AppConfig{
		"foo": appCfg[0],
	}

	unknownApp := "bar"
	app := apps["foo"].App

	keyPair := NewKeyPair()

	visorCfg := Config{
		KeyPair:       keyPair,
		AppServerAddr: appcommon.DefaultServerAddr,
	}

	visor := &Visor{
		router:   r,
		appsConf: apps,
		logger:   logging.MustGetLogger("test"),
		conf:     &visorCfg,
	}

	require.NoError(t, pathutil.EnsureDir(visor.dir()))

	defer func() {
		require.NoError(t, os.RemoveAll(visor.dir()))
	}()

	appCfg1 := appcommon.Config{
		Name:       app,
		ServerAddr: appcommon.DefaultServerAddr,
		VisorPK:    visorCfg.Keys().PubKey.Hex(),
		WorkDir:    filepath.Join("", app),
	}

	appArgs1 := append([]string{filepath.Join(visor.dir(), app)}, apps["foo"].Args...)
	appPID1 := appcommon.ProcID(10)

	pm := &appserver.MockProcManager{}
	pm.On("Start", mock.Anything, appCfg1, appArgs1, mock.Anything, mock.Anything).
		Return(appPID1, testhelpers.NoErr)
	pm.On("Wait", app).Return(testhelpers.NoErr)
	pm.On("Stop", app).Return(testhelpers.NoErr)
	pm.On("Exists", app).Return(true)
	pm.On("Exists", unknownApp).Return(false)

	visor.procManager = pm

	rpc := &RPC{visor: visor, log: logrus.New()}

	err = rpc.StartApp(&unknownApp, nil)
	require.Error(t, err)
	assert.Equal(t, ErrUnknownApp, err)

	require.NoError(t, rpc.StartApp(&app, nil))
	time.Sleep(100 * time.Millisecond)

	err = rpc.StopApp(&unknownApp, nil)
	require.Error(t, err)
	assert.Equal(t, ErrUnknownApp, err)

	require.NoError(t, rpc.StopApp(&app, nil))
	time.Sleep(100 * time.Millisecond)

	// remove files
	require.NoError(t, os.RemoveAll("foo"))
}

/*
TODO(evanlinjin): Fix these tests.
These tests have been commented out for the following reasons:
- We can't seem to get them to work.
- Mock transport causes too much issues so we deleted it.
*/

//func TestRPC(t *testing.T) {
//	r := new(mockRouter)
//	executer := new(MockExecuter)
//	defer func() {
//		require.NoError(t, os.RemoveAll("skychat"))
//	}()
//
//	pk1, _, tm1, tm2, errCh, err := transport.MockTransportManagersPair()
//
//	require.NoError(t, err)
//	defer func() {
//		require.NoError(t, tm1.Close())
//		require.NoError(t, tm2.Close())
//		require.NoError(t, <-errCh)
//		require.NoError(t, <-errCh)
//	}()
//
//	_, err = tm2.SaveTransport(context.TODO(), pk1, snet.DmsgType)
//	require.NoError(t, err)
//
//	apps := []AppConfig{
//		{App: "foo", AutoStart: false, Port: 10},
//		{App: "bar", AutoStart: false, Port: 20},
//	}
//	conf := &Config{}
//	conf.Visor.PubKey = pk1
//	visor := &Visor{
//		config:      conf,
//		router:      r,
//		tm:          tm1,
//		rt:          routing.New(),
//		executer:    executer,
//		appsConf:    apps,
//		startedApps: map[string]*appBind{},
//		logger:      logging.MustGetLogger("test"),
//	}
//	pathutil.EnsureDir(visor.dir())
//	defer func() {
//		if err := os.RemoveAll(visor.dir()); err != nil {
//			log.WithError(err).Warn(err)
//		}
//	}()
//
//	require.NoError(t, visor.StartApp("foo"))
//
//	time.Sleep(time.Second)
//	gateway := &RPC{visor: visor}
//
//	sConn, cConn := net.Pipe()
//	defer func() {
//		require.NoError(t, sConn.Close())
//		require.NoError(t, cConn.Close())
//	}()
//
//	svr := rpc.NewServer()
//	require.NoError(t, svr.RegisterName(RPCPrefix, gateway))
//	go svr.ServeConn(sConn)
//
//	// client := RPCClient{Client: rpc.NewClient(cConn)}
//	client := NewRPCClient(rpc.NewClient(cConn), "")
//
//	printFunc := func(t *testing.T, name string, v interface{}) {
//		j, err := json.MarshalIndent(v, name+": ", "  ")
//		require.NoError(t, err)
//		t.Log(string(j))
//	}
//
//	t.Run("Summary", func(t *testing.T) {
//		test := func(t *testing.T, summary *Summary) {
//			assert.Equal(t, pk1, summary.PubKey)
//			assert.Len(t, summary.Apps, 2)
//			assert.Len(t, summary.Transports, 1)
//			printFunc(t, "Summary", summary)
//		}
//		t.Run("RPCServer", func(t *testing.T) {
//			var summary Summary
//			require.NoError(t, gateway.Summary(&struct{}{}, &summary))
//			test(t, &summary)
//		})
//		t.Run("RPCClient", func(t *testing.T) {
//			summary, err := client.Summary()
//			require.NoError(t, err)
//			test(t, summary)
//		})
//	})
//
//	t.Run("Exec", func(t *testing.T) {
//		command := "echo 1"
//
//		t.Run("RPCServer", func(t *testing.T) {
//			var out []byte
//			require.NoError(t, gateway.Exec(&command, &out))
//			assert.Equal(t, []byte("1\n"), out)
//		})
//
//		t.Run("RPCClient", func(t *testing.T) {
//			out, err := client.Exec(command)
//			require.NoError(t, err)
//			assert.Equal(t, []byte("1\n"), out)
//		})
//	})
//
//	t.Run("Apps", func(t *testing.T) {
//		test := func(t *testing.T, apps []*AppState) {
//			assert.Len(t, apps, 2)
//			printFunc(t, "Apps", apps)
//		}
//		t.Run("RPCServer", func(t *testing.T) {
//			var apps []*AppState
//			require.NoError(t, gateway.Apps(&struct{}{}, &apps))
//			test(t, apps)
//		})
//		t.Run("RPCClient", func(t *testing.T) {
//			apps, err := client.Apps()
//			require.NoError(t, err)
//			test(t, apps)
//		})
//	})
//
//	// TODO(evanlinjin): For some reason, this freezes.
//	t.Run("StopStartApp", func(t *testing.T) {
//		appName := "foo"
//		require.NoError(t, gateway.StopApp(&appName, &struct{}{}))
//		require.NoError(t, gateway.StartApp(&appName, &struct{}{}))
//		require.NoError(t, client.StopApp(appName))
//		require.NoError(t, client.StartApp(appName))
//	})
//
//	t.Run("SetAutoStart", func(t *testing.T) {
//		unknownAppName := "whoAmI"
//		appName := "foo"
//
//		in1 := SetAutoStartIn{AppName: unknownAppName, AutoStart: true}
//		in2 := SetAutoStartIn{AppName: appName, AutoStart: true}
//		in3 := SetAutoStartIn{AppName: appName, AutoStart: false}
//
//		// Test with RPC Server
//
//		err := gateway.SetAutoStart(&in1, &struct{}{})
//		require.Error(t, err)
//		assert.Equal(t, ErrUnknownApp, err)
//
//		require.NoError(t, gateway.SetAutoStart(&in2, &struct{}{}))
//		assert.True(t, visor.appsConf[0].AutoStart)
//
//		require.NoError(t, gateway.SetAutoStart(&in3, &struct{}{}))
//		assert.False(t, visor.appsConf[0].AutoStart)
//
//		// Test with RPC Client
//
//		err = client.SetAutoStart(in1.AppName, in1.AutoStart)
//		require.Error(t, err)
//		assert.Equal(t, ErrUnknownApp.Error(), err.Error())
//
//		require.NoError(t, client.SetAutoStart(in2.AppName, in2.AutoStart))
//		assert.True(t, visor.appsConf[0].AutoStart)
//
//		require.NoError(t, client.SetAutoStart(in3.AppName, in3.AutoStart))
//		assert.False(t, visor.appsConf[0].AutoStart)
//	})
//
//	t.Run("TransportTypes", func(t *testing.T) {
//		in := TransportsIn{ShowLogs: true}
//
//		var out []*TransportSummary
//		require.NoError(t, gateway.Transports(&in, &out))
//		require.Len(t, out, 1)
//		assert.Equal(t, "mock", out[0].Type)
//
//		out2, err := client.Transports(in.FilterTypes, in.FilterPubKeys, in.ShowLogs)
//		require.NoError(t, err)
//		assert.Equal(t, out, out2)
//	})
//
//	t.Run("Transport", func(t *testing.T) {
//		var ids []uuid.UUID
//		visor.tm.WalkTransports(func(tp *transport.ManagedTransport) bool {
//			ids = append(ids, tp.RuleEntry.ID)
//			return true
//		})
//
//		for _, id := range ids {
//			id := id
//			var summary TransportSummary
//			require.NoError(t, gateway.Transport(&id, &summary))
//
//			summary2, err := client.Transport(id)
//			require.NoError(t, err)
//			require.Equal(t, summary, *summary2)
//		}
//	})
//
//	// TODO: Test add/remove transports
//
//}
