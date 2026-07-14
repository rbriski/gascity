package effectinventory

import (
	"context"
	"strings"
	"testing"
)

func TestDiscoverProfileRejectsCloseOnOpenWorldCompatibleChannel(t *testing.T) {
	const packageName = "escapehard/closeparam"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	boundary := fixtureEscapeChannelBoundary(packageName, "Approved")

	_, err := discoverProfile(context.Background(), config, fixtureLinuxProfile(), []BoundaryDefinition{boundary})
	assertRejectedEscape(t, err, "CloseInjected", "close")
}

func TestDiscoverProfileRejectsCallSliceAndBoundReflectCall(t *testing.T) {
	const packageName = "escapehard/reflectcall"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	boundaries := []BoundaryDefinition{
		{
			ID:   "escapehard.reflectcall.effect",
			Kind: KindProviderMutation,
			Object: ObjectRef{
				Package: fixtureModulePath + "/" + packageName,
				Name:    "Effect",
			},
			Match: ObjectMatchExact,
		},
		{
			ID:   "escapehard.reflectcall.variadic-effect",
			Kind: KindProviderMutation,
			Object: ObjectRef{
				Package: fixtureModulePath + "/" + packageName,
				Name:    "VariadicEffect",
			},
			Match: ObjectMatchExact,
		},
	}

	_, err := discoverProfile(context.Background(), config, fixtureLinuxProfile(), boundaries)
	assertRejectedEscape(t, err, "SliceInvoke", "ReflectBoundCall", "reflect")
}

func TestDiscoverProfileRejectsReflectiveChannelOperations(t *testing.T) {
	const packageName = "escapehard/reflectchannel"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	boundary := fixtureEscapeChannelBoundary(packageName, "Approved")

	_, err := discoverProfile(context.Background(), config, fixtureLinuxProfile(), []BoundaryDefinition{boundary})
	assertRejectedEscape(t, err,
		"ReflectSend",
		"ReflectTrySend",
		"ReflectRecv",
		"ReflectTryRecv",
		"ReflectClose",
		"ReflectSelectSend",
		"ReflectSelectRecv",
		"reflect",
	)
}

func TestDiscoverProfileRejectsUnsafeDerivedChannelEffect(t *testing.T) {
	const packageName = "escapehard/unsafechannel"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	boundary := fixtureEscapeChannelBoundary(packageName, "Approved")

	_, err := discoverProfile(context.Background(), config, fixtureLinuxProfile(), []BoundaryDefinition{boundary})
	assertRejectedEscape(t, err, "UnsafeDerivedSend", "unsafe")
}

func fixtureLinuxProfile() analysisProfile {
	return analysisProfile{ID: BuildLinuxDefault, GOOS: "linux", GOARCH: "amd64"}
}

func fixtureEscapeChannelBoundary(packageName, objectName string) BoundaryDefinition {
	return BoundaryDefinition{
		ID:   "escapehard." + strings.ReplaceAll(packageName, "/", ".") + ".channel",
		Kind: KindWakeSource,
		Object: ObjectRef{
			Package: fixtureModulePath + "/" + packageName,
			Name:    objectName,
		},
		Match: ObjectMatchChannel,
	}
}

func assertRejectedEscape(t *testing.T, err error, want ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("discoverProfile() returned nil, want fail-closed escape diagnostic")
	}
	for _, fragment := range want {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("discoverProfile() error = %q, want %q", err, fragment)
		}
	}
}
