const RAW = 'https://raw.githubusercontent.com/kyc-rip/cli/master/scripts/install.sh';

export default {
  async fetch(req) {
    const url = new URL(req.url);
    if (url.pathname === '/install.sh') {
      const r = await fetch(RAW, { cf: { cacheTtl: 300, cacheEverything: true } });
      if (!r.ok) return new Response('installer fetch failed\n', { status: 502 });
      return new Response(r.body, {
        status: 200,
        headers: {
          'content-type': 'text/plain; charset=utf-8',
          'cache-control': 'public, max-age=300',
          'x-served-by': 'kyc-cli-installer',
        },
      });
    }
    // Everything else → canonical landing on swap.kyc.rip
    return Response.redirect('https://swap.kyc.rip' + url.pathname + url.search, 301);
  },
};
