# test list comprehension

print([x.upper() for x in ["one", "two", "three", "four", "five", "six"] if len(x) <= 4])