package controllers_test

import (
	_ "embed"
	"testing"

	"games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	"games-on-whales.github.io/direwolf/pkg/controllers"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
)

//go:embed firefox.yaml
var firefox string

func TestConfig(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.Install(scheme); err != nil {
		t.Fatal(err)
	}

	codecs := serializer.NewCodecFactory(scheme)
	res, _, err := codecs.UniversalDeserializer().Decode([]byte(firefox), nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	t.Log("Successfully decoded the config")

	tomls, err := controllers.GenerateWolfConfig(res.(*v1alpha1.App))
	if err != nil {
		t.Fatal(err)
	}

	t.Log("Successfully generated the config")
	t.Log(tomls)

}
