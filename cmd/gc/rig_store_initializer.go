package main

import "github.com/gastownhall/gascity/internal/rig"

type cliRigStoreProvisioner struct{}

func (cliRigStoreProvisioner) InitRigStore(cityPath, dir, prefix string) (bool, error) {
	return initDirIfReady(cityPath, dir, prefix)
}

func (cliRigStoreProvisioner) PrepareAdoptedRigStore(cityPath, rigPath string) error {
	return prepareRigAdoptProviderState(cityPath, rigPath)
}

func (cliRigStoreProvisioner) InitAndHookRigStore(cityPath, dir, prefix string) error {
	return initAndHookDir(cityPath, dir, prefix)
}

type controllerRigStoreProvisioner struct{}

func (controllerRigStoreProvisioner) InitRigStore(cityPath, dir, prefix string) (bool, error) {
	return controllerStateInitRigDirIfReady(cityPath, dir, prefix)
}

func (controllerRigStoreProvisioner) PrepareAdoptedRigStore(cityPath, rigPath string) error {
	return prepareRigAdoptProviderState(cityPath, rigPath)
}

func (controllerRigStoreProvisioner) InitAndHookRigStore(cityPath, dir, prefix string) error {
	return initAndHookDir(cityPath, dir, prefix)
}

var (
	_ rig.StoreProvisioner = cliRigStoreProvisioner{}
	_ rig.StoreProvisioner = controllerRigStoreProvisioner{}
)
