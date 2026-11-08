// Generates the DOM-stub harness from index.html so live-ui-check.js always
// exercises the page's current JavaScript.
const fs = require('fs');
const path = require('path');
const here = path.dirname(__filename);
const html = fs.readFileSync(path.join(here, 'index.html'), 'utf8');
const js = html.split('<script>')[1].split('</script>')[0];
const harness = `
globalThis.__dom = {};
const document = {
  getElementById: (id) => (globalThis.__dom[id] = globalThis.__dom[id] || { innerHTML:'', textContent:'', hidden:false, value:'', dataset:{}, addEventListener(){}, click(){}, insertAdjacentHTML(pos,html){ this.innerHTML += html; } }),
  addEventListener(){}, title:'' };
const setInterval = () => {};
const fetch = async (u, o) => globalThis.__fetch(u, o);
`;
const out = harness + js.replace('refresh();',
  'globalThis.__api = {loadHead, loadBlocks, showBlock, showTx, showAccount, showDetail}; globalThis.__ready = refresh();');
fs.writeFileSync(path.join(require('os').tmpdir(), 'explorer_harness.js'), out);
console.log('harness written');
