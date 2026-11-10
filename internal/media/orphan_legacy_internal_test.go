package media

import (
	"testing"

	"github.com/Willh92/Volo-go/internal/domain"
)

func TestLegacyRelativeImageRefsMatchRootObjectKey(t *testing.T) {
	asset := domain.MediaAsset{ObjectKey: "php_20190706210324.png", PublicURL: "/uploads/php_20190706210324.png"}
	keys := map[string]bool{}
	for _, k := range AssetCandidateKeys(asset) {
		keys[k] = true
	}
	blobs := []string{
		`![](image/php_20190706210324.png)`,
		`<img src="./image/php_20190706210324.png">`,
		`![x](/posts/image/php_20190706210324.png?imageView2/2/w/600)`,
		`![x](https://www.willh.cn/images/php_20190706210324.png)`,
		`![x](../upload/php_20190706210324.png)`,
	}
	for _, blob := range blobs {
		hit := false
		emitNormalizedTokensFromBlob(blob, func(k string) {
			if keys[k] {
				hit = true
			}
		})
		if !hit {
			t.Errorf("legacy ref %q not matched to asset %q", blob, asset.ObjectKey)
		}
	}
	if got := stripLegacyImagePrefix("image/../etc/passwd"); got != "" {
		t.Errorf("expected traversal to be rejected, got %q", got)
	}
	if got := stripLegacyImagePrefix("/uploads/x.png"); got != "" {
		t.Errorf("expected /uploads/ to be left alone, got %q", got)
	}
}
