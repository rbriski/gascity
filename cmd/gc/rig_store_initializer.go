package main

import "github.com/gastownhall/gascity/internal/rig"

type cliRigStoreInitializer struct{}

func (cliRigStoreInitializer) InitRigStore(cityPath, dir, prefix string) (bool, error) {
	return initDirIfReady(cityPath, dir, prefix)
}

type controllerRigStoreInitializer struct{}

func (controllerRigStoreInitializer) InitRigStore(cityPath, dir, prefix string) (bool, error) {
	return controllerStateInitRigDirIfReady(cityPath, dir, prefix)
}

var (
	_ rig.StoreInitializer = cliRigStoreInitializer{}
	_ rig.StoreInitializer = controllerRigStoreInitializer{}
)
