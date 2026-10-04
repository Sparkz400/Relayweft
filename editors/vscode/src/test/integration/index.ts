// The entry point VS Code's test host calls (--extensionTestsPath): runs
// the mocha suite inside the extension host.

import * as path from 'path';
import Mocha = require('mocha');

export function run(): Promise<void> {
  const mocha = new Mocha({ ui: 'bdd', timeout: 180_000, color: true, bail: true });
  mocha.addFile(path.join(__dirname, 'extension.it.js'));
  return new Promise((resolve, reject) => {
    mocha.run((failures) => (failures ? reject(new Error(`${failures} integration test(s) failed`)) : resolve()));
  });
}
