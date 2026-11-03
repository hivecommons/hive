package main

import "github.com/hivecommons/hive/pkg/github/automerge"

func (b *boot) basePreparationReader() func(string, string) (automerge.BasePreparationSnapshot, error) {
	reader := automerge.New(nil, automerge.Options{
		RequireCurrentBase: func(repo string) bool {
			return b.cfg != nil && b.cfg.AutoMerge.RequireCurrentBaseFor(repo)
		},
		StageLabelPolicy: stageLabelPolicyFunc(b.cfg),
	})
	return reader.BasePreparationSnapshot
}
