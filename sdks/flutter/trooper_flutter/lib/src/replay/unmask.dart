import 'package:flutter/widgets.dart';

/// Shows the text of [child]'s subtree in session replay (text is masked by default, `maskAllText`).
/// Text fields and obscured text stay masked regardless.
///
/// ```dart
/// TrooperUnmask(child: Text('Order total'))
/// ```
class TrooperUnmask extends StatelessWidget {
  const TrooperUnmask({super.key, required this.child, this.unmask = true});

  final Widget child;

  /// `false` masks the subtree again (e.g. a sensitive part inside an unmasked screen).
  final bool unmask;

  @override
  Widget build(BuildContext context) => child;
}
