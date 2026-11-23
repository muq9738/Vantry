package pi

import (
	"context"

	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/Vantry/pkg/provider"
)

// Models serves the conversation-independent model query used by the MacBook
// desktop. Full provider/model identities cross this API; credentials do not.
func (p *Provider) Models(ctx context.Context) ([]api.ModelInfo, error) {
	rawExe := p.rawPiExecutable
	if rawExe == "" {
		rawExe = p.options.PiExecutable
	}
	catalog, err := Catalog(ctx, ResolveExecutable(rawExe, p.options.DataDir), "", "")
	if err != nil {
		return nil, err
	}
	models := append([]api.ModelInfo(nil), catalog.Models...)
	for i := range models {
		models[i].ID = models[i].Key()
	}
	return models, nil
}

var _ provider.ModelCatalog = (*Provider)(nil)
