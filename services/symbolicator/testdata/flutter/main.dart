class PriceValidator {
  int validate(int price) {
    if (price < 0) {
      throw StateError('negative price: $price');
    }
    return price;
  }
}

class CartService {
  final PriceValidator validator = PriceValidator();

  @pragma('vm:never-inline')
  int computeTotal(List<int> items) {
    var sum = 0;
    for (final it in items) {
      sum += validator.validate(it);
    }
    return sum;
  }
}

void main(List<String> args) {
  try {
    print(CartService().computeTotal([1, 2, args.isEmpty ? -3 : 3]));
  } catch (e, st) {
    print(st);
  }
}
