// Fallback thumbnails for type icons across the ESP admin pages.
//
// /typeicon/{typeID}.png serves the client's own icon where one exists:
// eveGraphics.icon for items, otherwise the rendered snapshot in
// cache/Pictures/Gids. That snapshot cache only holds graphics the client has
// actually drawn — a couple of hundred out of ~2200 — so most ships and
// entities come back 404 and the row shows an empty box.
//
// Where that happens and a converted .glb exists, render the thumbnail in the
// browser instead. Requires npc_viewer.js (window.renderModelThumb) to be
// loaded first; without it this script is a no-op.
//
// Usage: mark the img with data-thumb-type="{typeID}" and include this file.
(function () {
  'use strict';

  var observer = null;

  function fill(img) {
    var typeID = img.getAttribute('data-thumb-type');
    if (!typeID || !window.renderModelThumb) return;
    var gen = window.MODEL_GEN || 0;
    window.renderModelThumb('/api/model/' + typeID + '.glb?g=' + gen, 96).then(function (dataURL) {
      if (!dataURL) return;
      img.src = dataURL;
      img.style.display = '';
      img.style.visibility = '';
    });
  }

  function queue(img) {
    if (!('IntersectionObserver' in window)) { fill(img); return; }
    if (!observer) {
      observer = new IntersectionObserver(function (entries) {
        entries.forEach(function (en) {
          if (!en.isIntersecting) return;
          observer.unobserve(en.target);
          fill(en.target);
        });
      }, { rootMargin: '300px' });
    }
    observer.observe(img);
  }

  function scan() {
    if (!window.renderModelThumb) return;
    var imgs = document.querySelectorAll('img[data-thumb-type]');
    for (var i = 0; i < imgs.length; i++) {
      var img = imgs[i];
      if (img.dataset.thumbDone === '1') continue;
      // complete + zero intrinsic width means the request failed.
      if (img.complete && img.naturalWidth === 0) {
        img.dataset.thumbDone = '1';
        img.removeAttribute('src');
        queue(img);
      } else if (!img.complete) {
        img.dataset.thumbDone = '1';
        img.addEventListener('error', function () {
          this.removeAttribute('src');
          queue(this);
        }, { once: true });
      }
    }
  }

  if (document.readyState === 'complete') scan();
  else window.addEventListener('load', scan);

  window.rescanTypeThumbs = scan;
})();
