// React Native CLI autolinking config for the TrooperRum native module (.cjs: this package is "type": "module").
module.exports = {
  dependency: {
    platforms: {
      android: {
        sourceDir: './android',
        packageImportPath: 'import dev.tasktrooper.reactnative.TrooperRumPackage;',
        packageInstance: 'new TrooperRumPackage()',
      },
      ios: {
        podspecPath: __dirname + '/trooper-react-native.podspec',
      },
    },
  },
};
