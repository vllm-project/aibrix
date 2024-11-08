
class MyClass:
    def generator(self, yieldable: bool):
        print("start")
        for i in range(10):
            print(f"{i} ")
            i += 1
            if yieldable:
                yield


gen = None
for i in range(10):
    print(f"iterate {i} ")
    if gen == None:
        c = MyClass()
        gen = c.generator(True)
    next(gen)